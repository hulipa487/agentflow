// Package mongodb implements the builtin:mongodb memory backend provider.
// It provides kv, prefix_scan, and ttl features via a MongoDB server.
//
// Requires a MongoDB 4.0+ server. The connection URI is set in the backend
// config under "url" (e.g. "mongodb://localhost:27017"). A database name is
// set under "database" (defaults to "agentflow"). Each logical table maps to
// a MongoDB collection.
package mongodb

import (
	"context"
	"fmt"
	"time"

	"agentflow/internal/core/memory"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Provider implements memory.BackendProvider for "builtin:mongodb".
type Provider struct{}

func (Provider) Name() string { return "builtin:mongodb" }

func (Provider) Features() []string {
	return []string{"kv", "prefix_scan", "ttl"}
}

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	uri, _ := config["url"].(string)
	if uri == "" {
		return nil, fmt.Errorf("builtin:mongodb: url is required")
	}
	dbName, _ := config["database"].(string)
	if dbName == "" {
		dbName = "agentflow"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("builtin:mongodb: connect: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("builtin:mongodb: ping: %w", err)
	}
	return &Handle{client: client, db: client.Database(dbName)}, nil
}

type Handle struct {
	client *mongo.Client
	db     *mongo.Database
}

// doc is the stored record shape. The ID is the logical key; the table name
// is the collection name.
type doc struct {
	ID        string    `bson:"_id"`
	Value     any       `bson:"value"`
	UpdatedAt int64     `bson:"updated_at"`
	ExpiresAt *int64    `bson:"expires_at,omitempty"`
}

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coll := h.db.Collection(table)
	now := time.Now().Unix()
	var expires *int64
	if opts.TTL > 0 {
		exp := now + int64(opts.TTL.Seconds())
		expires = &exp
	}
	_, err := coll.UpdateOne(ctx,
		bson.M{"_id": key},
		bson.M{"$set": doc{ID: key, Value: value, UpdatedAt: now, ExpiresAt: expires}},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coll := h.db.Collection(table)
	var d doc
	if err := coll.FindOne(ctx, bson.M{"_id": key}).Decode(&d); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, false, nil
		}
		return nil, false, err
	}
	if d.ExpiresAt != nil && *d.ExpiresAt <= time.Now().Unix() {
		return nil, false, nil
	}
	return d.Value, true, nil
}

func (h *Handle) Delete(table, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.db.Collection(table).DeleteOne(ctx, bson.M{"_id": key})
	return err
}

func (h *Handle) Query(table string, q memory.Query) (memory.Iterator, error) {
	switch q.Kind {
	case "prefix":
		return h.queryPrefix(table, q.Prefix)
	case "all":
		return h.queryAll(table)
	default:
		return nil, fmt.Errorf("builtin:mongodb: unsupported query kind %q", q.Kind)
	}
}

func (h *Handle) queryPrefix(table, prefix string) (memory.Iterator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	coll := h.db.Collection(table)
	filter := bson.M{"_id": bson.M{"$regex": "^" + prefix}}
	cur, err := coll.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	return &mongoIter{cur: cur}, nil
}

func (h *Handle) queryAll(table string) (memory.Iterator, error) {
	return h.queryPrefix(table, "")
}

func (h *Handle) GC(table string, window int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	coll := h.db.Collection(table)
	// Delete expired.
	if _, err := coll.DeleteMany(ctx, bson.M{
		"expires_at": bson.M{"$ne": nil, "$lte": time.Now().Unix()},
	}); err != nil {
		return err
	}
	// Window: keep only the most recent N by updated_at.
	if window <= 0 {
		return nil
	}
	count, err := coll.CountDocuments(ctx, bson.M{})
	if err != nil {
		return err
	}
	if count <= int64(window) {
		return nil
	}
	// Find the cutoff row and delete everything older.
	skip := int64(window)
	cur, err := coll.Find(ctx, bson.M{}, options.Find().
		SetSort(bson.D{{Key: "updated_at", Value: -1}}).
		SetSkip(skip).
		SetLimit(1))
	if err != nil {
		return err
	}
	var cutoff doc
	if cur.Next(ctx) {
		if err := cur.Decode(&cutoff); err != nil {
			cur.Close(ctx)
			return err
		}
		cur.Close(ctx)
		_, err = coll.DeleteMany(ctx, bson.M{"updated_at": bson.M{"$lt": cutoff.UpdatedAt}})
		return err
	}
	cur.Close(ctx)
	return nil
}

func (h *Handle) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.client.Disconnect(ctx)
}

type mongoIter struct {
	cur   *mongo.Cursor
	rec   memory.Record
	err   error
	ready bool
}

func (it *mongoIter) Next() bool {
	if it.err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !it.cur.Next(ctx) {
		it.err = it.cur.Err()
		return false
	}
	var d doc
	if err := it.cur.Decode(&d); err != nil {
		it.err = err
		return false
	}
	it.rec = memory.Record{Key: d.ID, Value: d.Value}
	return true
}

func (it *mongoIter) Record() memory.Record { return it.rec }
func (it *mongoIter) Err() error {
	if it.err != nil {
		return it.err
	}
	return it.cur.Err()
}
