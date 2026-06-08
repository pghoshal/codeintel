// Package search implements the headless code search backend used
// by codeintel-app. This first slice is intentionally Zoekt-only:
// semantic and graph enrichment are layered after this path has
// real transport, tenant metadata, and response normalization.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/db"
	"codeintel/pkg/repoindexstatus"

	zoektpb "github.com/sourcegraph/zoekt/grpc/protos/zoekt/webserver/v1"
	zoektquery "github.com/sourcegraph/zoekt/query"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	MetadataOrgID    = "codeintel-org-id"
	MetadataTenantID = "x-sourcegraph-tenant-id"

	DefaultMatches      = 30
	MaxMatches          = 1000
	MaxContextLines     = 50
	defaultDialTimeout  = 5 * time.Second
	defaultQueryTimeout = 10 * time.Second
	defaultGRPCMaxBytes = 500 * 1024 * 1024

	shardWarmupMaxAttempts = 8
)

var shardWarmupRetryDelay = func(attempt int) time.Duration {
	return 250 * time.Millisecond
}

type RepoLookup interface {
	ListOrgSearchRepos(ctx context.Context, orgID int32, repoIDs []int32, repoNames []string) ([]db.SearchRepoRow, error)
	ListOrgSearchPolicyRepos(ctx context.Context, orgID int32, repoNames []string) ([]db.SearchRepoRow, error)
}

type ZoektClient interface {
	Search(ctx context.Context, in *zoektpb.SearchRequest, opts ...grpc.CallOption) (*zoektpb.SearchResponse, error)
}

type ClientFactory func(ctx context.Context, endpoint string, cfg Config) (ZoektClient, error)

type Config struct {
	Endpoints     []string
	Replicated    bool
	DialTimeout   time.Duration
	QueryTimeout  time.Duration
	GRPCMaxBytes  int
	RepoLookup    RepoLookup
	ClientFactory ClientFactory
}

type Backend struct {
	cfg     Config
	clients []endpointClient
}

type endpointClient struct {
	endpoint string
	client   ZoektClient
}

func NewBackend(ctx context.Context, cfg Config) (*Backend, error) {
	cfg.Endpoints = normalizeEndpoints(cfg.Endpoints)
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("%w: no Zoekt endpoints configured", api.ErrSearchBackendNotConfigured)
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = defaultQueryTimeout
	}
	if cfg.GRPCMaxBytes <= 0 {
		cfg.GRPCMaxBytes = defaultGRPCMaxBytes
	}
	if cfg.ClientFactory == nil {
		cfg.ClientFactory = dialZoektClient
	}
	if cfg.RepoLookup == nil {
		return nil, fmt.Errorf("%w: repo lookup not configured", api.ErrSearchBackendNotConfigured)
	}

	dialCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()

	clients := make([]endpointClient, 0, len(cfg.Endpoints))
	for _, endpoint := range cfg.Endpoints {
		client, err := cfg.ClientFactory(dialCtx, endpoint, cfg)
		if err != nil {
			return nil, fmt.Errorf("%w: dial %s: %v", api.ErrSearchBackendUnavailable, endpoint, err)
		}
		clients = append(clients, endpointClient{endpoint: endpoint, client: client})
	}
	return &Backend{cfg: cfg, clients: clients}, nil
}

func (b *Backend) Search(ctx context.Context, req api.SearchRequest) (json.RawMessage, error) {
	if b == nil || len(b.clients) == 0 {
		return nil, api.ErrSearchBackendNotConfigured
	}
	queryCtx, cancel := context.WithTimeout(ctx, b.cfg.QueryTimeout)
	defer cancel()

	policyFilter, empty, err := b.resolveBranchPolicyFilter(queryCtx, req)
	if err != nil {
		return nil, err
	}
	if empty {
		return marshalSearchResponse(emptySearchResponse())
	}
	planned, err := planZoektRequest(req, b.cfg.QueryTimeout, policyFilter)
	if err != nil {
		return nil, err
	}
	queryCtx = metadata.AppendToOutgoingContext(
		queryCtx,
		MetadataOrgID, strconv.FormatInt(int64(req.OrgID), 10),
		MetadataTenantID, strconv.FormatInt(int64(req.OrgID), 10),
	)

	var normalized searchResponse
	for attempt := 0; ; attempt++ {
		responses, err := b.executeSearch(queryCtx, planned)
		if err != nil {
			return nil, err
		}

		normalized, err = b.normalize(queryCtx, req.OrgID, req.OrgDomain, responses, policyFilter)
		if err != nil {
			return nil, err
		}
		if !shouldRetryShardWarmup(normalized, attempt) {
			break
		}
		if err := sleepContext(queryCtx, shardWarmupRetryDelay(attempt)); err != nil {
			break
		}
	}
	return marshalSearchResponse(normalized)
}

func marshalSearchResponse(resp searchResponse) (json.RawMessage, error) {
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshal search response: %w", err)
	}
	return out, nil
}

func emptySearchResponse() searchResponse {
	return searchResponse{
		Stats:              searchStats{},
		Files:              []searchFile{},
		RepositoryInfo:     []repositoryInfo{},
		IsSearchExhaustive: true,
	}
}

func (b *Backend) executeSearch(ctx context.Context, req *zoektpb.SearchRequest) ([]*zoektpb.SearchResponse, error) {
	if b.cfg.Replicated {
		return b.searchFirstAvailable(ctx, req)
	}
	return b.searchFanout(ctx, req)
}

func shouldRetryShardWarmup(resp searchResponse, attempt int) bool {
	if attempt >= shardWarmupMaxAttempts-1 {
		return false
	}
	return resp.Stats.ActualMatchCount == 0 &&
		resp.Stats.ShardsScanned == 0 &&
		resp.Stats.ShardFilesConsidered == 0
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *Backend) searchFirstAvailable(ctx context.Context, req *zoektpb.SearchRequest) ([]*zoektpb.SearchResponse, error) {
	var lastErr error
	for _, client := range b.clients {
		resp, err := client.client.Search(ctx, req)
		if err == nil {
			return []*zoektpb.SearchResponse{resp}, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%w: all replicated Zoekt endpoints failed: %v", api.ErrSearchBackendUnavailable, lastErr)
}

func (b *Backend) searchFanout(ctx context.Context, req *zoektpb.SearchRequest) ([]*zoektpb.SearchResponse, error) {
	responses := make([]*zoektpb.SearchResponse, len(b.clients))
	group, groupCtx := errgroup.WithContext(ctx)
	for i, client := range b.clients {
		i, client := i, client
		group.Go(func() error {
			resp, err := client.client.Search(groupCtx, req)
			if err != nil {
				return fmt.Errorf("%w: Zoekt endpoint %s failed: %v", api.ErrSearchBackendUnavailable, client.endpoint, err)
			}
			responses[i] = resp
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return responses, nil
}

func dialZoektClient(ctx context.Context, endpoint string, cfg Config) (ZoektClient, error) {
	target, err := grpcTarget(endpoint)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(cfg.GRPCMaxBytes),
			grpc.MaxCallSendMsgSize(cfg.GRPCMaxBytes),
		),
	)
	if err != nil {
		return nil, err
	}
	return zoektpb.NewWebserverServiceClient(conn), nil
}

func grpcTarget(endpoint string) (string, error) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return "", errors.New("empty endpoint")
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", err
		}
		if parsed.Host == "" {
			return "", fmt.Errorf("endpoint %q has no host", endpoint)
		}
		return parsed.Host, nil
	}
	return trimmed, nil
}

func normalizeEndpoints(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, endpoint := range raw {
		for _, piece := range strings.Split(endpoint, ",") {
			trimmed := strings.TrimSpace(piece)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			out = append(out, trimmed)
		}
	}
	return out
}

type searchResponse struct {
	Stats              searchStats      `json:"stats"`
	Files              []searchFile     `json:"files"`
	RepositoryInfo     []repositoryInfo `json:"repositoryInfo"`
	IsSearchExhaustive bool             `json:"isSearchExhaustive"`
}

type searchStats struct {
	ActualMatchCount      int64  `json:"actualMatchCount"`
	TotalMatchCount       int64  `json:"totalMatchCount"`
	Duration              int64  `json:"duration"`
	FileCount             int64  `json:"fileCount"`
	FilesSkipped          int64  `json:"filesSkipped"`
	ContentBytesLoaded    int64  `json:"contentBytesLoaded"`
	IndexBytesLoaded      int64  `json:"indexBytesLoaded"`
	Crashes               int64  `json:"crashes"`
	ShardFilesConsidered  int64  `json:"shardFilesConsidered"`
	FilesConsidered       int64  `json:"filesConsidered"`
	FilesLoaded           int64  `json:"filesLoaded"`
	ShardsScanned         int64  `json:"shardsScanned"`
	ShardsSkipped         int64  `json:"shardsSkipped"`
	ShardsSkippedFilter   int64  `json:"shardsSkippedFilter"`
	NgramMatches          int64  `json:"ngramMatches"`
	NgramLookups          int64  `json:"ngramLookups"`
	Wait                  int64  `json:"wait"`
	MatchTreeConstruction int64  `json:"matchTreeConstruction"`
	MatchTreeSearch       int64  `json:"matchTreeSearch"`
	RegexpsConsidered     int64  `json:"regexpsConsidered"`
	FlushReason           string `json:"flushReason"`
}

type repositoryInfo struct {
	ID           int32   `json:"id"`
	CodeHostType string  `json:"codeHostType"`
	Name         string  `json:"name"`
	DisplayName  *string `json:"displayName,omitempty"`
	WebURL       *string `json:"webUrl,omitempty"`

	enforceRevisionVisibility bool
	visibleRevisionNames      map[string]bool
}

type searchFile struct {
	FileName       fileNameMatch `json:"fileName"`
	WebURL         string        `json:"webUrl"`
	ExternalWebURL *string       `json:"externalWebUrl,omitempty"`
	Repository     string        `json:"repository"`
	RepositoryID   int32         `json:"repositoryId"`
	Language       string        `json:"language"`
	Chunks         []searchChunk `json:"chunks"`
	Branches       []string      `json:"branches,omitempty"`
	Content        *string       `json:"content,omitempty"`
}

type fileNameMatch struct {
	Text        string        `json:"text"`
	MatchRanges []sourceRange `json:"matchRanges"`
}

type searchChunk struct {
	Content      string        `json:"content"`
	MatchRanges  []sourceRange `json:"matchRanges"`
	ContentStart location      `json:"contentStart"`
	Symbols      []symbolInfo  `json:"symbols,omitempty"`
}

type symbolInfo struct {
	Symbol string      `json:"symbol"`
	Kind   string      `json:"kind"`
	Parent *symbolInfo `json:"parent,omitempty"`
}

type sourceRange struct {
	Start location `json:"start"`
	End   location `json:"end"`
}

type location struct {
	ByteOffset uint32 `json:"byteOffset"`
	LineNumber uint32 `json:"lineNumber"`
	Column     uint32 `json:"column"`
}

type repoKey struct {
	id   int32
	name string
}

type branchPolicyFilter struct {
	pairs []repoBranchPair
}

type repoBranchPair struct {
	repoName string
	branch   string
}

func (b *Backend) resolveBranchPolicyFilter(ctx context.Context, req api.SearchRequest) (*branchPolicyFilter, bool, error) {
	if b.cfg.RepoLookup == nil {
		return nil, false, nil
	}
	queryBranches := extractQueryBranchFilters(req.Query)
	optionBranches := extractOptionBranchFilters(req.Options)

	repoNames := stringListOption(req.Options, "repoSearchScope")
	rows, err := b.cfg.RepoLookup.ListOrgSearchPolicyRepos(ctx, req.OrgID, repoNames)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, true, nil
	}

	requiredBranches := queryBranches
	useAllVisibleBranches := false
	if len(requiredBranches) == 0 {
		requiredBranches = optionBranches
	}
	if len(requiredBranches) == 0 {
		useAllVisibleBranches = true
	}
	pairs := make([]repoBranchPair, 0, len(rows)*len(requiredBranches))
	seen := map[string]bool{}
	enforcedRows := 0
	for _, row := range rows {
		visible, enforced := visibleBranchNamesForSearch(row)
		if !enforced {
			if useAllVisibleBranches {
				key := row.Name + "\x00HEAD"
				if !seen[key] {
					seen[key] = true
					pairs = append(pairs, repoBranchPair{repoName: row.Name, branch: "HEAD"})
				}
				continue
			}
			for _, branch := range requiredBranches {
				key := row.Name + "\x00" + branch
				if seen[key] {
					continue
				}
				seen[key] = true
				pairs = append(pairs, repoBranchPair{repoName: row.Name, branch: branch})
			}
			continue
		}
		enforcedRows++
		if len(visible) == 0 {
			continue
		}
		if useAllVisibleBranches {
			branch := defaultVisibleBranchNameForSearch(row, visible)
			if branch == "" {
				continue
			}
			key := row.Name + "\x00" + branch
			if seen[key] {
				continue
			}
			seen[key] = true
			pairs = append(pairs, repoBranchPair{repoName: row.Name, branch: branch})
			continue
		}
		for _, branch := range requiredBranches {
			if !visible[branch] {
				continue
			}
			key := row.Name + "\x00" + branch
			if seen[key] {
				continue
			}
			seen[key] = true
			pairs = append(pairs, repoBranchPair{repoName: row.Name, branch: branch})
		}
	}
	if enforcedRows == 0 {
		return nil, false, nil
	}
	if len(pairs) == 0 {
		return nil, true, nil
	}
	return &branchPolicyFilter{pairs: pairs}, false, nil
}

func (f *branchPolicyFilter) allows(repoName string, branches []string) bool {
	if f == nil || len(f.pairs) == 0 {
		return true
	}
	if repoName == "" || len(branches) == 0 {
		return false
	}
	for _, pair := range f.pairs {
		if pair.repoName != repoName {
			continue
		}
		for _, branch := range branches {
			normalized := stripSearchRefPrefix(branch)
			if pair.branch == branch || pair.branch == normalized {
				return true
			}
		}
	}
	return false
}

func visibleBranchNamesForSearch(row db.SearchRepoRow) (map[string]bool, bool) {
	info := repositoryInfo{}
	addRevisionVisibility(&info, row)
	if !info.enforceRevisionVisibility {
		return nil, false
	}
	out := make(map[string]bool, len(info.visibleRevisionNames))
	for branch := range info.visibleRevisionNames {
		if branch == "" || branch == "HEAD" || strings.HasPrefix(branch, "refs/") {
			continue
		}
		out[branch] = true
	}
	return out, true
}

func defaultVisibleBranchNameForSearch(row db.SearchRepoRow, visible map[string]bool) string {
	if row.DefaultBranch != nil && *row.DefaultBranch != "" && visible[*row.DefaultBranch] {
		return *row.DefaultBranch
	}
	branches := make([]string, 0, len(visible))
	for branch := range visible {
		if branch == "" || branch == "HEAD" || strings.HasPrefix(branch, "refs/") {
			continue
		}
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	if len(branches) == 0 {
		return ""
	}
	return branches[0]
}

func (b *Backend) normalize(ctx context.Context, orgID int32, domain string, responses []*zoektpb.SearchResponse, policyFilter *branchPolicyFilter) (searchResponse, error) {
	_ = domain
	repoIDs, repoNames := collectRepoRefs(responses)
	repos, err := b.lookupRepos(ctx, orgID, repoIDs, repoNames)
	if err != nil {
		return searchResponse{}, err
	}

	var out searchResponse
	repoInfoByKey := map[string]repositoryInfo{}
	filtered := false
	for _, resp := range responses {
		out.Stats = accumulateStats(out.Stats, resp.GetStats())
		for _, file := range resp.GetFiles() {
			if !policyFilter.allows(file.GetRepository(), file.GetBranches()) {
				filtered = true
				continue
			}
			normalized, info, ok := normalizeFile(file, repos, b.cfg.RepoLookup != nil)
			if !ok {
				filtered = true
				continue
			}
			out.Files = append(out.Files, normalized)
			repoInfoByKey[repositoryInfoKey(info)] = info
		}
	}
	out.Stats.ActualMatchCount = countActualMatches(out.Files)
	if filtered {
		out.Stats.TotalMatchCount = out.Stats.ActualMatchCount
	} else if out.Stats.TotalMatchCount == 0 {
		out.Stats.TotalMatchCount = out.Stats.ActualMatchCount
	}
	out.Stats.FileCount = int64(len(out.Files))
	out.IsSearchExhaustive = out.Stats.TotalMatchCount <= out.Stats.ActualMatchCount
	out.RepositoryInfo = make([]repositoryInfo, 0, len(repoInfoByKey))
	for _, info := range repoInfoByKey {
		out.RepositoryInfo = append(out.RepositoryInfo, info)
	}
	return out, nil
}

func repositoryInfoKey(info repositoryInfo) string {
	if info.ID > 0 {
		return "id:" + strconv.FormatInt(int64(info.ID), 10)
	}
	return "name:" + info.Name
}

func collectRepoRefs(responses []*zoektpb.SearchResponse) ([]int32, []string) {
	ids := map[int32]bool{}
	names := map[string]bool{}
	for _, resp := range responses {
		for _, file := range resp.GetFiles() {
			if file.GetRepositoryId() > 0 {
				ids[int32(file.GetRepositoryId())] = true
			} else if file.GetRepository() != "" {
				names[file.GetRepository()] = true
			}
		}
	}
	idList := make([]int32, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	nameList := make([]string, 0, len(names))
	for name := range names {
		nameList = append(nameList, name)
	}
	return idList, nameList
}

func (b *Backend) lookupRepos(ctx context.Context, orgID int32, repoIDs []int32, repoNames []string) (map[repoKey]repositoryInfo, error) {
	out := map[repoKey]repositoryInfo{}
	if b.cfg.RepoLookup == nil {
		for _, id := range repoIDs {
			out[repoKey{id: id}] = repositoryInfo{ID: id, CodeHostType: "generic-git-host"}
		}
		for _, name := range repoNames {
			out[repoKey{name: name}] = repositoryInfo{Name: name, CodeHostType: "generic-git-host"}
		}
		return out, nil
	}
	rows, err := b.cfg.RepoLookup.ListOrgSearchRepos(ctx, orgID, repoIDs, repoNames)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		info := repositoryInfo{
			ID:           row.ID,
			CodeHostType: row.CodeHostType,
			Name:         row.Name,
			DisplayName:  row.DisplayName,
			WebURL:       row.WebURL,
		}
		addRevisionVisibility(&info, row)
		out[repoKey{id: row.ID}] = info
		out[repoKey{name: row.Name}] = info
	}
	return out, nil
}

func addRevisionVisibility(info *repositoryInfo, row db.SearchRepoRow) {
	if searchBlockedByRemoveIndex(row) {
		info.enforceRevisionVisibility = true
		info.visibleRevisionNames = map[string]bool{}
		return
	}
	if row.IndexedAt == nil {
		info.enforceRevisionVisibility = true
		info.visibleRevisionNames = map[string]bool{}
		return
	}
	if len(row.Metadata) == 0 {
		return
	}
	md := repoindexstatus.ParseMetadata(row.Metadata)
	indexedAt := ""
	indexedAt = row.IndexedAt.UTC().Format(time.RFC3339Nano)
	repoInput := repoindexstatus.RepoInput{
		Metadata:      row.Metadata,
		DefaultBranch: row.DefaultBranch,
	}
	if indexedAt != "" {
		repoInput.IndexedAt = &indexedAt
	}
	visible := repoindexstatus.GetPolicyVisibleIndexedRevisions(repoInput)
	if len(visible) == 0 && row.IndexedAt != nil && (len(md.Branches) > 0 || row.DefaultBranch != nil) {
		if row.DefaultBranch != nil && *row.DefaultBranch != "" && repoindexstatus.IsBranchAllowedByIndexPolicy(repoInput, *row.DefaultBranch) {
			visible = []string{repoindexstatus.BranchRevision(*row.DefaultBranch)}
		}
	}
	if len(visible) == 0 && len(md.Branches) == 0 && row.DefaultBranch == nil {
		return
	}
	info.enforceRevisionVisibility = true
	info.visibleRevisionNames = map[string]bool{}
	for _, revision := range visible {
		addVisibleRevisionName(info.visibleRevisionNames, revision)
	}
	if row.DefaultBranch != nil && *row.DefaultBranch != "" && info.visibleRevisionNames[*row.DefaultBranch] {
		info.visibleRevisionNames["HEAD"] = true
	}
}

func searchBlockedByRemoveIndex(row db.SearchRepoRow) bool {
	if row.LatestJobType == nil || row.LatestJobStatus == nil {
		return false
	}
	if *row.LatestJobType != "REMOVE_INDEX" {
		return false
	}
	switch *row.LatestJobStatus {
	case "PENDING", "IN_PROGRESS", "FAILED":
		return true
	default:
		return false
	}
}

func addVisibleRevisionName(out map[string]bool, revision string) {
	if revision == "" {
		return
	}
	out[revision] = true
	if strings.HasPrefix(revision, "refs/heads/") {
		out[strings.TrimPrefix(revision, "refs/heads/")] = true
		return
	}
	if strings.HasPrefix(revision, "refs/tags/") {
		out[strings.TrimPrefix(revision, "refs/tags/")] = true
	}
}

func normalizeFile(file *zoektpb.FileMatch, repos map[repoKey]repositoryInfo, requireRepositoryID bool) (searchFile, repositoryInfo, bool) {
	var (
		info repositoryInfo
		ok   bool
	)
	if file.GetRepositoryId() > 0 {
		// A repository id from the search engine is authoritative.
		// If that id is not valid for the authenticated org, never
		// fall back to name. Same repo names can exist across orgs;
		// name fallback here would relabel another tenant's hit as
		// the caller's repo and leak file content.
		info, ok = repos[repoKey{id: int32(file.GetRepositoryId())}]
		if !ok {
			return searchFile{}, repositoryInfo{}, false
		}
	} else {
		if requireRepositoryID {
			return searchFile{}, repositoryInfo{}, false
		}
		info, ok = repos[repoKey{name: file.GetRepository()}]
		if !ok {
			return searchFile{}, repositoryInfo{}, false
		}
	}
	if info.ID == 0 {
		info.ID = int32(file.GetRepositoryId())
	}
	if info.Name == "" {
		info.Name = file.GetRepository()
	}
	if info.CodeHostType == "" {
		info.CodeHostType = "generic-git-host"
	}
	if !fileBranchesVisible(file.GetBranches(), info) {
		return searchFile{}, repositoryInfo{}, false
	}

	name := string(file.GetFileName())
	var nameRanges []sourceRange
	var chunks []searchChunk
	for _, chunk := range file.GetChunkMatches() {
		if chunk.GetFileName() {
			nameRanges = append(nameRanges, convertRanges(chunk.GetRanges())...)
			continue
		}
		chunks = append(chunks, searchChunk{
			Content:      string(chunk.GetContent()),
			MatchRanges:  convertRanges(chunk.GetRanges()),
			ContentStart: convertLocation(chunk.GetContentStart()),
			Symbols:      convertSymbols(chunk.GetSymbolInfo()),
		})
	}
	var content *string
	if len(file.GetContent()) > 0 {
		value := string(file.GetContent())
		content = &value
	}
	return searchFile{
		FileName: fileNameMatch{
			Text:        name,
			MatchRanges: nameRanges,
		},
		WebURL:       "",
		Repository:   info.Name,
		RepositoryID: info.ID,
		Language:     file.GetLanguage(),
		Chunks:       chunks,
		Branches:     file.GetBranches(),
		Content:      content,
	}, info, true
}

func fileBranchesVisible(branches []string, info repositoryInfo) bool {
	if !info.enforceRevisionVisibility {
		return true
	}
	if len(info.visibleRevisionNames) == 0 {
		return false
	}
	if len(branches) == 0 {
		return false
	}
	for _, branch := range branches {
		normalized := stripSearchRefPrefix(branch)
		if info.visibleRevisionNames[branch] || info.visibleRevisionNames[normalized] {
			return true
		}
	}
	return false
}

func convertRanges(in []*zoektpb.Range) []sourceRange {
	out := make([]sourceRange, 0, len(in))
	for _, r := range in {
		out = append(out, sourceRange{
			Start: convertLocation(r.GetStart()),
			End:   convertLocation(r.GetEnd()),
		})
	}
	return out
}

func convertLocation(in *zoektpb.Location) location {
	if in == nil {
		return location{LineNumber: 1, Column: 1}
	}
	line := in.GetLineNumber()
	if line == 0 {
		line = 1
	}
	column := in.GetColumn()
	if column == 0 {
		column = 1
	}
	return location{
		ByteOffset: in.GetByteOffset(),
		LineNumber: line,
		Column:     column,
	}
}

func convertSymbols(in []*zoektpb.SymbolInfo) []symbolInfo {
	out := make([]symbolInfo, 0, len(in))
	for _, s := range in {
		if s == nil {
			continue
		}
		item := symbolInfo{Symbol: s.GetSym(), Kind: s.GetKind()}
		if s.GetParent() != "" || s.GetParentKind() != "" {
			item.Parent = &symbolInfo{Symbol: s.GetParent(), Kind: s.GetParentKind()}
		}
		out = append(out, item)
	}
	return out
}

func countActualMatches(files []searchFile) int64 {
	var total int64
	for _, file := range files {
		total += int64(len(file.FileName.MatchRanges))
		for _, chunk := range file.Chunks {
			total += int64(len(chunk.MatchRanges))
		}
	}
	return total
}

func accumulateStats(acc searchStats, stats *zoektpb.Stats) searchStats {
	if stats == nil {
		return acc
	}
	acc.TotalMatchCount += stats.GetMatchCount()
	acc.Duration += durationNanos(stats.GetDuration())
	acc.FilesSkipped += stats.GetFilesSkipped()
	acc.ContentBytesLoaded += stats.GetContentBytesLoaded()
	acc.IndexBytesLoaded += stats.GetIndexBytesLoaded()
	acc.Crashes += stats.GetCrashes()
	acc.ShardFilesConsidered += stats.GetShardFilesConsidered()
	acc.FilesConsidered += stats.GetFilesConsidered()
	acc.FilesLoaded += stats.GetFilesLoaded()
	acc.ShardsScanned += stats.GetShardsScanned()
	acc.ShardsSkipped += stats.GetShardsSkipped()
	acc.ShardsSkippedFilter += stats.GetShardsSkippedFilter()
	acc.NgramMatches += stats.GetNgramMatches()
	acc.NgramLookups += stats.GetNgramLookups()
	acc.Wait += durationNanos(stats.GetWait())
	acc.MatchTreeConstruction += durationNanos(stats.GetMatchTreeConstruction())
	acc.MatchTreeSearch += durationNanos(stats.GetMatchTreeSearch())
	acc.RegexpsConsidered += stats.GetRegexpsConsidered()
	acc.FlushReason = stats.GetFlushReason().String()
	return acc
}

func durationNanos(d *durationpb.Duration) int64 {
	if d == nil {
		return 0
	}
	return d.AsDuration().Nanoseconds()
}

func planZoektRequest(req api.SearchRequest, timeout time.Duration, policyFilter *branchPolicyFilter) (*zoektpb.SearchRequest, error) {
	parsed, err := zoektquery.Parse(normalizeRevisionFilters(req.Query))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", api.ErrSearchInvalidQuery, err)
	}
	parsed = applyScope(parsed, req.Options, policyFilter != nil)
	parsed = applyBranchPolicyFilter(parsed, policyFilter)
	opts := parseOptions(req.Options, timeout)
	return &zoektpb.SearchRequest{
		Query: zoektquery.QToProto(parsed),
		Opts:  opts,
	}, nil
}

func applyBranchPolicyFilter(q zoektquery.Q, policyFilter *branchPolicyFilter) zoektquery.Q {
	if policyFilter == nil || len(policyFilter.pairs) == 0 {
		return q
	}
	reposByBranch := make(map[string]map[string]bool)
	for _, pair := range policyFilter.pairs {
		if pair.repoName == "" || pair.branch == "" {
			continue
		}
		if reposByBranch[pair.branch] == nil {
			reposByBranch[pair.branch] = map[string]bool{}
		}
		reposByBranch[pair.branch][pair.repoName] = true
	}
	branchNames := make([]string, 0, len(reposByBranch))
	for branch := range reposByBranch {
		branchNames = append(branchNames, branch)
	}
	sort.Strings(branchNames)
	branches := make([]zoektquery.Q, 0, len(branchNames))
	for _, branch := range branchNames {
		branches = append(branches, zoektquery.NewAnd(
			&zoektquery.RepoSet{Set: reposByBranch[branch]},
			&zoektquery.Branch{Pattern: branch, Exact: true},
		))
	}
	if len(branches) == 0 {
		return q
	}
	return zoektquery.Simplify(zoektquery.NewAnd(q, zoektquery.NewOr(branches...)))
}

func normalizeRevisionFilters(query string) string {
	var out strings.Builder
	inQuote := false
	escaped := false
	for i := 0; i < len(query); {
		c := query[i]
		if inQuote {
			out.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inQuote = false
			}
			i++
			continue
		}
		if c == '"' {
			inQuote = true
			out.WriteByte(c)
			i++
			continue
		}
		prefix, ok := searchBranchFilterPrefix(query, i)
		if ok && isSearchTokenBoundary(query, i) {
			valueStart := i + len(prefix)
			value, end, quoted := readSearchFilterValue(query, valueStart)
			ref := stripSearchRefPrefix(value)
			out.WriteString("branch:")
			if quoted {
				out.WriteByte('"')
			}
			out.WriteString(ref)
			if quoted {
				out.WriteByte('"')
			}
			i = end
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

func extractQueryBranchFilters(query string) []string {
	out := make([]string, 0, 1)
	seen := map[string]bool{}
	inQuote := false
	escaped := false
	for i := 0; i < len(query); {
		c := query[i]
		if inQuote {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inQuote = false
			}
			i++
			continue
		}
		if c == '"' {
			inQuote = true
			i++
			continue
		}
		prefix, ok := searchBranchFilterPrefix(query, i)
		if ok && isSearchTokenBoundary(query, i) {
			if isSearchNegatedFilter(query, i) {
				_, end, _ := readSearchFilterValue(query, i+len(prefix))
				i = end
				continue
			}
			value, end, _ := readSearchFilterValue(query, i+len(prefix))
			branch := stripSearchRefPrefix(value)
			if branch != "" && !seen[branch] {
				seen[branch] = true
				out = append(out, branch)
			}
			i = end
			continue
		}
		i++
	}
	return out
}

func extractOptionBranchFilters(options map[string]any) []string {
	branches := stringListOption(options, "branches")
	if len(branches) == 0 {
		if branch := stringOption(options, "branch"); branch != "" {
			branches = []string{branch}
		}
	}
	out := make([]string, 0, len(branches))
	seen := map[string]bool{}
	for _, branch := range branches {
		normalized := stripSearchRefPrefix(branch)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
}

func searchBranchFilterPrefix(query string, i int) (string, bool) {
	for _, prefix := range []string{"branch:", "rev:", "b:"} {
		if strings.HasPrefix(query[i:], prefix) {
			return prefix, true
		}
	}
	return "", false
}

func readSearchFilterValue(query string, start int) (string, int, bool) {
	if start >= len(query) {
		return "", start, false
	}
	if query[start] != '"' {
		end := start
		for end < len(query) && !isSearchTokenSeparator(query[end]) {
			end++
		}
		return query[start:end], end, false
	}
	var out strings.Builder
	escaped := false
	for i := start + 1; i < len(query); i++ {
		c := query[i]
		if escaped {
			out.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			return out.String(), i + 1, true
		}
		out.WriteByte(c)
	}
	return out.String(), len(query), true
}

func isSearchTokenBoundary(query string, i int) bool {
	if i == 0 {
		return true
	}
	prev := query[i-1]
	return isSearchTokenSeparator(prev) || prev == '-' || prev == '('
}

func isSearchNegatedFilter(query string, i int) bool {
	return i > 0 && query[i-1] == '-'
}

func isSearchTokenSeparator(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ')':
		return true
	default:
		return false
	}
}

func stripSearchRefPrefix(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref
}

func applyScope(q zoektquery.Q, options map[string]any, policyScoped bool) zoektquery.Q {
	var scoped []zoektquery.Q
	scoped = append(scoped, q)

	if !containsBranch(q) && !policyScoped {
		branches := stringListOption(options, "branches")
		if len(branches) == 0 {
			if branch := stringOption(options, "branch"); branch != "" {
				branches = []string{branch}
			}
		}
		if len(branches) == 0 {
			branches = []string{"HEAD"}
		}
		if len(branches) == 1 {
			scoped = append(scoped, &zoektquery.Branch{Pattern: stripSearchRefPrefix(branches[0]), Exact: true})
		} else {
			branchQueries := make([]zoektquery.Q, 0, len(branches))
			for _, branch := range branches {
				branchQueries = append(branchQueries, &zoektquery.Branch{Pattern: stripSearchRefPrefix(branch), Exact: true})
			}
			scoped = append(scoped, zoektquery.NewOr(branchQueries...))
		}
	}

	if repos := stringListOption(options, "repoSearchScope"); len(repos) > 0 {
		set := map[string]bool{}
		for _, repo := range repos {
			set[repo] = true
		}
		scoped = append(scoped, &zoektquery.RepoSet{Set: set})
	}
	return zoektquery.Simplify(zoektquery.NewAnd(scoped...))
}

func containsBranch(q zoektquery.Q) bool {
	found := false
	zoektquery.VisitAtoms(q, func(atom zoektquery.Q) {
		if _, ok := atom.(*zoektquery.Branch); ok {
			found = true
		}
	})
	return found
}

func parseOptions(options map[string]any, timeout time.Duration) *zoektpb.SearchOptions {
	matches := int64Option(options, "matches", DefaultMatches)
	if matches == DefaultMatches {
		matches = int64Option(options, "count", DefaultMatches)
	}
	if matches < 1 {
		matches = DefaultMatches
	}
	if matches > MaxMatches {
		matches = MaxMatches
	}
	contextLines := int64Option(options, "contextLines", 0)
	if contextLines < 0 {
		contextLines = 0
	}
	if contextLines > MaxContextLines {
		contextLines = MaxContextLines
	}
	return &zoektpb.SearchOptions{
		ChunkMatches:         true,
		MaxMatchDisplayCount: matches,
		TotalMaxMatchCount:   matches + 1,
		NumContextLines:      contextLines,
		Whole:                boolOption(options, "whole"),
		ShardMaxMatchCount:   -1,
		MaxWallTime:          durationpb.New(timeout),
	}
}

func int64Option(options map[string]any, key string, fallback int64) int64 {
	if options == nil {
		return fallback
	}
	switch v := options[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return parsed
		}
	case string:
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func boolOption(options map[string]any, key string) bool {
	if options == nil {
		return false
	}
	switch v := options[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	default:
		return false
	}
}

func stringOption(options map[string]any, key string) string {
	if options == nil {
		return ""
	}
	v, _ := options[key].(string)
	return strings.TrimSpace(v)
}

func stringListOption(options map[string]any, key string) []string {
	if options == nil {
		return nil
	}
	switch v := options[key].(type) {
	case []string:
		return cleanStrings(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return cleanStrings(out)
	case string:
		return cleanStrings(strings.Split(v, ","))
	default:
		return nil
	}
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}
