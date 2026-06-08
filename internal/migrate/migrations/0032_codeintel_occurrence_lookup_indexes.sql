-- Fast graph inspection seed lookup over SCIP occurrences. Full line-content
-- recall belongs to Zoekt; graph inspection should use symbol/path indexes.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS "CodeIntelOccurrence_symbol_trgm_idx"
  ON "CodeIntelOccurrence"
  USING GIN (symbol gin_trgm_ops);

CREATE INDEX IF NOT EXISTS "CodeIntelOccurrence_filePath_trgm_idx"
  ON "CodeIntelOccurrence"
  USING GIN ("filePath" gin_trgm_ops);
