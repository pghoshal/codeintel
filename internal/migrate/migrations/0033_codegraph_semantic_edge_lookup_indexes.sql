CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticEdge_lookupText_trgm_idx"
    ON "CodeGraphSemanticEdge"
    USING gin ((
        COALESCE("sourceExternalId", '') || ' ' ||
        COALESCE("targetExternalId", '') || ' ' ||
        COALESCE(relation, '') || ' ' ||
        COALESCE("sourceFile", '') || ' ' ||
        COALESCE(evidence, '') || ' ' ||
        COALESCE(rationale, '')
    ) gin_trgm_ops);

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticEdge_org_graph_source_relation_rank_idx"
    ON "CodeGraphSemanticEdge" ("orgId", "graphIndexId", source, relation, confidence DESC, "sourceFile", "startLine");
