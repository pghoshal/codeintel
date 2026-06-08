-- Fast exact and suffix symbol lookup for MCP SCIP definition/reference tools.
-- Real repositories can have millions of occurrences; symbol tools must not
-- devolve into full CodeIntelSymbol scans when codegraph_context probes several
-- symbols for one question.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS "CodeIntelSymbol_displayName_trgm_idx"
  ON "CodeIntelSymbol"
  USING GIN ("displayName" gin_trgm_ops);

CREATE INDEX IF NOT EXISTS "CodeIntelSymbol_symbol_trgm_idx"
  ON "CodeIntelSymbol"
  USING GIN (symbol gin_trgm_ops);

CREATE INDEX IF NOT EXISTS "CodeIntelSymbol_orgId_repoId_lower_displayName_idx"
  ON "CodeIntelSymbol" ("orgId", "repoId", lower("displayName"));

CREATE INDEX IF NOT EXISTS "CodeIntelSymbol_orgId_repoId_lower_symbol_idx"
  ON "CodeIntelSymbol" ("orgId", "repoId", lower(symbol));
