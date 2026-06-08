// Type-alias bridge: pkg/dbpool is the canonical home for the pgx
// pool factory because BOTH binaries (codeintel-app + codeintel-
// backend) construct one. internal/db re-exports the same symbols
// so existing app-side callers (`db.NewPool`, `db.Pool`,
// `db.Config`) keep compiling without touching every site.
//
// New code that needs only the pool (e.g. codeintel-backend's
// minimal audit-event INSERT path) should import codeintel/pkg/
// dbpool directly. App-side code that also needs the typed query
// helpers (apikeys, secrets, connections, ...) imports
// codeintel/internal/db.
package db

import "codeintel/pkg/dbpool"

type Pool = dbpool.Pool
type Config = dbpool.Config

var NewPool = dbpool.NewPool
var ErrDSNRequired = dbpool.ErrDSNRequired
var ErrInsecureRemoteDSN = dbpool.ErrInsecureRemoteDSN
