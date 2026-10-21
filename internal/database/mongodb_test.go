package database

import "testing"

func TestAnalyzeMongo(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		kind      Kind
		unbounded bool
		refused   bool
	}{
		{"find", `{"find": "users", "filter": {"age": {"$gt": 30}}, "limit": 10}`, KindRead, false, false},
		{"find case insensitive", `{"Find": "users"}`, KindRead, false, false},
		{"aggregate", `{"aggregate": "o", "pipeline": [{"$match": {}}], "cursor": {}}`, KindRead, false, false},
		{"aggregate out", `{"aggregate": "o", "pipeline": [{"$out": "copy"}], "cursor": {}}`, KindWrite, true, false},
		{"aggregate nested merge", `{"aggregate": "o", "pipeline": [{"$facet": {"a": [{"$merge": "x"}]}}], "cursor": {}}`, KindWrite, true, false},
		{"count", `{"count": "users", "query": {}}`, KindRead, false, false},
		{"explain find", `{"explain": {"find": "users"}}`, KindRead, false, false},
		{"explain delete", `{"explain": {"delete": "users", "deletes": [{"q": {}, "limit": 0}]}, "verbosity": "executionStats"}`, KindWrite, true, false},
		{"where operator", `{"find": "users", "filter": {"$where": "sleep(100000)"}}`, KindSchema, false, false},
		{"function in aggregate", `{"aggregate": "u", "pipeline": [{"$addFields": {"x": {"$function": {"body": "1", "args": [], "lang": "js"}}}}], "cursor": {}}`, KindSchema, false, false},

		{"insert", `{"insert": "users", "documents": [{"name": "a"}]}`, KindWrite, false, false},
		{"update keyed", `{"update": "users", "updates": [{"q": {"_id": 1}, "u": {"$set": {"a": 1}}}]}`, KindWrite, false, false},
		{"update empty filter", `{"update": "users", "updates": [{"q": {}, "u": {"$set": {"a": 1}}, "multi": true}]}`, KindWrite, true, false},
		{"update exists filter", `{"update": "users", "updates": [{"q": {"_id": {"$exists": true}}, "u": {"$set": {"a": 1}}}]}`, KindWrite, true, false},
		{"update ne null", `{"update": "users", "updates": [{"q": {"a": {"$ne": null}}, "u": {"$set": {"a": 1}}}]}`, KindWrite, true, false},
		{"update null equality", `{"update": "users", "updates": [{"q": {"a": null}, "u": {"$set": {"a": 1}}}]}`, KindWrite, true, false},
		{"update minkey range", `{"update": "users", "updates": [{"q": {"a": {"$gt": {"$minKey": 1}}}, "u": {"$set": {"a": 1}}}]}`, KindWrite, true, false},
		{"update expr", `{"update": "users", "updates": [{"q": {"$expr": true}, "u": {"$set": {"a": 1}}}]}`, KindWrite, true, false},
		{"update or with empty", `{"update": "users", "updates": [{"q": {"$or": [{"_id": 1}, {}]}, "u": {"$set": {"a": 1}}}]}`, KindWrite, true, false},
		{"update and selective", `{"update": "users", "updates": [{"q": {"$and": [{"_id": 1}, {"a": {"$exists": true}}]}, "u": {"$set": {"a": 1}}}]}`, KindWrite, false, false},
		{"update upsert", `{"update": "users", "updates": [{"q": {"_id": 1}, "u": {"$set": {"a": 1}}, "upsert": true}]}`, KindWrite, true, false},
		{"update pipeline", `{"update": "users", "updates": [{"q": {"_id": 1}, "u": [{"$set": {"a": 1}}]}]}`, KindWrite, true, false},
		{"update one bad of two", `{"update": "users", "updates": [{"q": {"_id": 1}, "u": {"$set": {"a": 1}}}, {"q": {}, "u": {"$set": {"a": 1}}}]}`, KindWrite, true, false},
		{"delete keyed", `{"delete": "users", "deletes": [{"q": {"_id": 1}, "limit": 1}]}`, KindWrite, false, false},
		{"delete empty", `{"delete": "users", "deletes": [{"q": {}, "limit": 0}]}`, KindWrite, true, false},
		{"find and modify", `{"findAndModify": "users", "query": {"_id": 1}, "update": {"$set": {"a": 1}}}`, KindWrite, false, false},
		{"find and modify empty", `{"findAndModify": "users", "query": {}, "remove": true}`, KindWrite, true, false},

		{"drop", `{"drop": "users"}`, KindSchema, false, false},
		{"create index", `{"createIndexes": "users", "indexes": []}`, KindSchema, false, false},
		{"map reduce", `{"mapReduce": "users", "map": "f", "reduce": "g", "out": "x"}`, KindSchema, false, false},
		{"transaction", `{"startTransaction": 1}`, KindSession, false, true},
		{"unknown", `{"somethingNew": 1}`, KindUnknown, true, false},
		{"not json", `db.users.find({})`, KindUnknown, false, true},
		{"empty", `{}`, KindUnknown, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnalyzeMongo(tt.cmd)
			if got.Kind != tt.kind || got.Unbounded != tt.unbounded || got.Refused != tt.refused {
				t.Errorf("AnalyzeMongo(%s) = %+v, want kind=%s unbounded=%v refused=%v",
					tt.cmd, got, tt.kind, tt.unbounded, tt.refused)
			}
		})
	}
}

func TestMongoPermissions(t *testing.T) {
	// The property that matters: a read-only grant never runs a write, and
	// safe-write never runs a collection-wide one.
	for _, cmd := range []string{
		`{"delete": "users", "deletes": [{"q": {}, "limit": 0}]}`,
		`{"aggregate": "o", "pipeline": [{"$out": "users"}], "cursor": {}}`,
		`{"update": "users", "updates": [{"q": {"$or": [{}]}, "u": {"$set": {"a": 1}}}]}`,
	} {
		if _, err := AuthorizeAnalysis("m", AnalyzeMongo(cmd), PermissionSafeWrite); err == nil {
			t.Errorf("%s allowed with safe-write", cmd)
		}
	}

	if _, err := AuthorizeAnalysis("m", AnalyzeMongo(`{"insert": "u", "documents": [{}]}`), PermissionReadOnly); err == nil {
		t.Error("insert allowed with read-only")
	}
}
