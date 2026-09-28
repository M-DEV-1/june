package agent

import "sync/atomic"

// storeSchema holds the description of the store's own tables, columns and column vocabularies, read from the database at startup by db.DescribeSchema and handed here by the daemon.
// It is a package-level value rather than an argument because the tool list is rebuilt for every session from six call sites, all of which describe the same one store in the same one process. Writing it once at startup keeps those signatures unchanged.
var storeSchema atomic.Value

// SetStoreSchema records the store's live schema for the query_store tool description. Called once, at daemon startup, before any session opens.
func SetStoreSchema(s string) { storeSchema.Store(s) }

// storeSchemaBlock returns the recorded schema, or a line saying it is not available.
// A written schema is what this replaces: the hand-written one claimed notes.kind included "action_item" when the real value is "action", so a query counting open action items returned zero against twenty-five real ones. A schema read from the store cannot be wrong in that way.
func storeSchemaBlock() string {
	if s, ok := storeSchema.Load().(string); ok && s != "" {
		return s
	}
	return "(unavailable — use PRAGMA table_info(<table>) to read a table's columns)"
}
