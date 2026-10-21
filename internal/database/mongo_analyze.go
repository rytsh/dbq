package database

import (
	"errors"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// MongoDB statements are database commands written as Extended JSON, the
// same documents db.runCommand() takes:
//
//	{"find": "users", "filter": {"age": {"$gt": 30}}, "limit": 10}
//	{"aggregate": "orders", "pipeline": [{"$group": {"_id": "$status", "n": {"$sum": 1}}}]}
//	{"updateOne": ...} is not a command; use {"update": "users", "updates": [...]}
//
// Commands are a closed set, which makes classifying them a lookup rather than
// a parse: the command name is the first key of the document.

// mongoReadCommands only read.
var mongoReadCommands = map[string]bool{
	"find": true, "aggregate": true, "count": true, "distinct": true,
	"listcollections": true, "listindexes": true, "listdatabases": true,
	"collstats": true, "dbstats": true, "explain": true, "ping": true,
	"buildinfo": true, "hello": true, "ismaster": true, "serverstatus": true,
	"connectionstatus": true, "hostinfo": true, "validate": true,
	"datasize": true, "getparameter": true, "listsearchindexes": true,
}

// mongoWriteCommands modify documents and are measured for blast radius.
var mongoWriteCommands = map[string]bool{
	"insert": true, "update": true, "delete": true, "findandmodify": true,
}

// mongoSchemaCommands change collections, indexes, users or the server.
var mongoSchemaCommands = map[string]bool{
	"create": true, "drop": true, "dropdatabase": true, "createindexes": true,
	"dropindexes": true, "collmod": true, "renamecollection": true,
	"createuser": true, "dropuser": true, "updateuser": true, "grantrolestouser": true,
	"revokerolesfromuser": true, "createrole": true, "droprole": true,
	"compact": true, "reindex": true, "clonecollectionascapped": true,
	"converttocapped": true, "createsearchindexes": true, "dropsearchindex": true,
	"updatesearchindex": true, "shardcollection": true, "setparameter": true,
	"fsync": true, "shutdown": true, "killop": true, "killcursors": true,
	"killsessions": true, "killallsessions": true, "applyops": true,
	"setfeaturecompatibilityversion": true,
}

// mongoWriteStages are aggregation stages that write their output.
var mongoWriteStages = map[string]bool{"$out": true, "$merge": true}

// mongoCodeOperators run server-side JavaScript. A read built from them can
// still do arbitrary work, so they require full access.
var mongoCodeOperators = map[string]bool{"$where": true, "$function": true, "$accumulator": true}

// ParseMongoCommand decodes a command document and returns it with its name,
// lower-cased.
func ParseMongoCommand(statement string) (bson.D, string, error) {
	statement = strings.TrimSuffix(strings.TrimSpace(statement), ";")

	var doc bson.D
	if err := bson.UnmarshalExtJSON([]byte(statement), false, &doc); err != nil {
		return nil, "", errors.New(
			`a MongoDB statement is a command document in Extended JSON, e.g. {"find": "users", "filter": {}, "limit": 10}`,
		)
	}

	if len(doc) == 0 {
		return nil, "", errors.New("empty command document")
	}

	return doc, strings.ToLower(doc[0].Key), nil
}

// AnalyzeMongo classifies a MongoDB command document.
func AnalyzeMongo(statement string) Analysis {
	doc, name, err := ParseMongoCommand(statement)
	if err != nil {
		return Analysis{Kind: KindUnknown, Refused: true, Reason: err.Error(), Advice: "Send one command document"}
	}

	if code := findMongoKey(doc, mongoCodeOperators); code != "" {
		return Analysis{Kind: KindSchema, Reason: code + " runs server-side JavaScript"}
	}

	switch {
	case name == "explain":
		// explain wraps another command; with verbosity executionStats it
		// runs it, so it is as dangerous as what it wraps.
		inner, ok := doc[0].Value.(bson.D)
		if !ok || len(inner) == 0 {
			return Analysis{Kind: KindUnknown, Refused: true, Reason: "explain needs a command document"}
		}

		b, err := bson.MarshalExtJSON(inner, false, false)
		if err != nil {
			return Analysis{Kind: KindUnknown, Refused: true, Reason: "explain needs a command document"}
		}

		return AnalyzeMongo(string(b))

	case name == "aggregate":
		if stage := findMongoKey(doc, mongoWriteStages); stage != "" {
			return Analysis{Kind: KindWrite, Unbounded: true, Reason: "aggregate with " + stage + " writes its whole output"}
		}

		return Analysis{Kind: KindRead}

	case name == "mapreduce":
		// mapReduce runs JavaScript and may write an output collection.
		return Analysis{Kind: KindSchema, Reason: "mapReduce runs server-side JavaScript"}

	case mongoReadCommands[name]:
		return Analysis{Kind: KindRead}

	case mongoWriteCommands[name]:
		return analyzeMongoWrite(doc, name)

	case mongoSchemaCommands[name]:
		return Analysis{Kind: KindSchema}

	case name == "starttransaction" || name == "committransaction" || name == "aborttransaction" ||
		name == "endsessions" || name == "logout" || name == "authenticate":
		return Analysis{
			Kind: KindSession, Refused: true,
			Reason: name + " changes session state, which would leak into unrelated commands on a pooled connection",
			Advice: "Run it through a client that owns its own connection",
		}

	default:
		return Analysis{Kind: KindUnknown, Unbounded: true, Reason: "command " + doc[0].Key + " is not known to dbq"}
	}
}

// analyzeMongoWrite bounds insert, update, delete and findAndModify the way the
// SQL analysis bounds DML: inserts are bounded, and an update or delete must
// match on something and must not be multi-document with an empty filter.
func analyzeMongoWrite(doc bson.D, name string) Analysis {
	a := analyzeMongoWriteReach(doc, name)
	if a.Unbounded {
		a.Advice = "Use a filter that selects specific documents (for example by _id), or ask the user to raise this connection's permission level"
	}

	return a
}

func analyzeMongoWriteReach(doc bson.D, name string) Analysis {
	switch name {
	case "insert":
		return Analysis{Kind: KindWrite}

	case "findandmodify":
		query, _ := mongoField(doc, "query").(bson.D)
		if len(query) == 0 || mongoFilterIsTautology(query) {
			return Analysis{Kind: KindWrite, Unbounded: true, Reason: "findAndModify without a selective query picks an arbitrary document"}
		}

		if remove, _ := mongoField(doc, "remove").(bool); !remove {
			if _, pipeline := mongoField(doc, "update").(bson.A); pipeline {
				return Analysis{Kind: KindWrite, Unbounded: true, Reason: "findAndModify with an aggregation pipeline"}
			}
		}

		if upsert, _ := mongoField(doc, "upsert").(bool); upsert {
			return Analysis{Kind: KindWrite, Unbounded: true, Reason: "findAndModify with upsert"}
		}

		return Analysis{Kind: KindWrite}

	case "update", "delete":
		listKey, filterKey := "updates", "q"
		if name == "delete" {
			listKey = "deletes"
		}

		list, ok := mongoField(doc, listKey).(bson.A)
		if !ok || len(list) == 0 {
			return Analysis{Kind: KindWrite, Unbounded: true, Reason: name + " has no " + listKey + " list"}
		}

		for _, item := range list {
			if reason := mongoStatementUnbounded(name, item, filterKey); reason != "" {
				return Analysis{Kind: KindWrite, Unbounded: true, Reason: reason}
			}
		}

		return Analysis{Kind: KindWrite}
	}

	return Analysis{Kind: KindWrite, Unbounded: true, Reason: "unrecognised write command"}
}

func mongoStatementUnbounded(name string, item any, filterKey string) string {
	stmt, ok := item.(bson.D)
	if !ok {
		return name + " entries must be documents"
	}

	filter, _ := mongoField(stmt, filterKey).(bson.D)
	if len(filter) == 0 {
		return name + " with an empty filter affects every document"
	}

	if mongoFilterIsTautology(filter) {
		return name + " filter matches every document"
	}

	if name == "update" {
		if upsert, _ := mongoField(stmt, "upsert").(bool); upsert {
			return "update with upsert may insert as well as modify"
		}

		// An aggregation-pipeline update can compute anything from the
		// document; a plain update document is what bounded writes look like.
		if _, pipeline := mongoField(stmt, "u").(bson.A); pipeline {
			return "update with an aggregation pipeline"
		}
	}

	return ""
}

// mongoFilterIsTautology catches filters that look selective but match every
// document: {"$expr": true}, {"$or": [{}]}, {"_id": {"$exists": true}} and
// friends. The fields of a filter are ANDed, so it is a tautology only when
// every one of them is. Like the SQL predicate analysis it errs towards
// "unbounded" whenever it cannot tell.
func mongoFilterIsTautology(filter bson.D) bool {
	for _, e := range filter {
		if !mongoClauseIsTautology(e) {
			return false
		}
	}

	return true
}

func mongoClauseIsTautology(e bson.E) bool {
	switch e.Key {
	case "$and":
		// AND is selective when any member is.
		clauses, ok := mongoClauses(e.Value)
		if !ok {
			return true
		}

		for _, c := range clauses {
			if len(c) > 0 && !mongoFilterIsTautology(c) {
				return false
			}
		}

		return true

	case "$or":
		// OR matches everything when any member does.
		clauses, ok := mongoClauses(e.Value)
		if !ok || len(clauses) == 0 {
			return true
		}

		for _, c := range clauses {
			if len(c) == 0 || mongoFilterIsTautology(c) {
				return true
			}
		}

		return false
	}

	if strings.HasPrefix(e.Key, "$") {
		// $expr, $jsonSchema, $where, $nor, $text, $comment: either able to
		// encode a tautology the analysis cannot see through, or not a
		// restriction at all.
		return true
	}

	if e.Value == nil {
		// {"field": null} also matches every document without the field.
		return true
	}

	cond, ok := e.Value.(bson.D)
	if !ok {
		// {"field": value} is an equality match.
		return false
	}

	return mongoConditionIsTautology(cond)
}

// mongoClauses reads the member list of $and / $or.
func mongoClauses(v any) ([]bson.D, bool) {
	list, ok := v.(bson.A)
	if !ok {
		return nil, false
	}

	out := make([]bson.D, 0, len(list))

	for _, item := range list {
		d, ok := item.(bson.D)
		if !ok {
			return nil, false
		}

		out = append(out, d)
	}

	return out, true
}

// mongoConditionIsTautology reports whether a field condition matches (nearly)
// every document. A subdocument without operators is an exact match and so is
// selective; among operators only the comparisons that pin values count,
// because $exists, $ne, $nin, $not, $type and $regex routinely match almost
// everything.
func mongoConditionIsTautology(cond bson.D) bool {
	operators := false

	for _, op := range cond {
		if !strings.HasPrefix(op.Key, "$") {
			continue
		}

		operators = true

		switch op.Key {
		case "$eq":
			if op.Value == nil {
				continue
			}

			return false
		case "$in", "$all", "$size", "$elemMatch":
			return false
		case "$gt", "$gte", "$lt", "$lte":
			// Against MinKey or MaxKey a range covers every value.
			switch op.Value.(type) {
			case bson.MinKey, bson.MaxKey:
				continue
			}

			return false
		}
	}

	return operators
}

func mongoField(doc bson.D, key string) any {
	for _, e := range doc {
		if strings.EqualFold(e.Key, key) {
			return e.Value
		}
	}

	return nil
}

// findMongoKey searches a document tree for any key in keys and returns the
// first one found.
func findMongoKey(v any, keys map[string]bool) string {
	switch t := v.(type) {
	case bson.D:
		for _, e := range t {
			if keys[strings.ToLower(e.Key)] || keys[e.Key] {
				return e.Key
			}

			if k := findMongoKey(e.Value, keys); k != "" {
				return k
			}
		}
	case bson.A:
		for _, item := range t {
			if k := findMongoKey(item, keys); k != "" {
				return k
			}
		}
	}

	return ""
}
