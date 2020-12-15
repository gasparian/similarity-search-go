package db

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	cm "vector-search-go/common"
)

var (
	dbtimeOut, _          = strconv.Atoi(os.Getenv("DB_CLIENT_TIMEOUT"))
	createIndexMaxTime, _ = strconv.Atoi(os.Getenv("CREATE_INDEX_MAX_TIME"))
)

// GetDbClient creates client for talking to the mongodb
func GetDbClient(dbLocation string) (*MongoClient, error) {
	client, err := mongo.NewClient(options.Client().ApplyURI(dbLocation))
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dbtimeOut)*time.Second)
	defer cancel()
	err = client.Connect(ctx)
	if err != nil {
		return nil, err
	}

	err = client.Ping(ctx, readpref.Primary())
	if err != nil {
		return nil, err
	}
	mongodb := &MongoClient{
		Client: client,
	}
	return mongodb, nil
}

// Disconnect client from the context
func (mongodb *MongoClient) Disconnect() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dbtimeOut)*time.Second)
	defer cancel()
	mongodb.Client.Disconnect(ctx)
}

// GetDb returns database object
func (mongodb *MongoClient) GetDb(dbName string) *mongo.Database {
	return mongodb.Client.Database(dbName)
}

// CreateIndexesByFields just creates the new unique ascending
// indexes based on field name (type should be int)
func CreateIndexesByFields(coll *mongo.Collection, fields []string, unique bool) error {
	models := make([]mongo.IndexModel, len(fields))
	for i, field := range fields {
		models[i] = mongo.IndexModel{
			Keys: bson.M{
				field: 1,
			},
			Options: options.MergeIndexOptions(
				options.Index().SetBackground(true), // deprecated since mongodb 4.2
				options.Index().SetUnique(unique),
				options.Index().SetSparse(true),
			),
		}
	}
	opts := options.CreateIndexes().SetMaxTime(time.Duration(createIndexMaxTime) * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := coll.Indexes().CreateMany(ctx, models, opts)
	if err != nil {
		return err
	}
	return nil
}

// DropIndexByField sends command to drop the selected index;
// Input format should be in the following format: ""Some Field_1""
func DropIndexByField(coll *mongo.Collection, indexName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(createIndexMaxTime)*time.Second)
	defer cancel()
	_, err := coll.Indexes().DropOne(ctx, indexName)
	if err != nil {
		return err
	}
	return nil
}

// GetAggregation runs prepared aggregation pipeline in mongodb
func GetAggregation(coll *mongo.Collection, groupStage mongo.Pipeline) ([]bson.M, error) {
	opts := options.Aggregate().SetMaxTime(time.Duration(dbtimeOut) * time.Second)
	cursor, err := coll.Aggregate(context.TODO(), groupStage, opts)
	if err != nil {
		return nil, err
	}

	var results []bson.M
	if err = cursor.All(context.TODO(), &results); err != nil {
		return nil, err
	}
	return results, nil
}

// ConvertAggResult makes Vector from the bson from Mongo
func ConvertAggResult(inp interface{}) (cm.Vector, error) {
	val, ok := inp.(primitive.A)
	if !ok {
		return cm.Vector{}, errors.New("type conversion failed")
	}
	conv := cm.Vector{
		Values: make([]float64, len(val)),
		Size:   len(val),
	}
	for i := range conv.Values {
		v, ok := val[i].(float64)
		if !ok {
			return cm.Vector{}, errors.New("type conversion failed")
		}
		conv.Values[i] = v
	}
	return conv, nil
}

// GetAggregatedStats returns vectors with Mongo aggregation results (mean and std vectors)
func GetAggregatedStats(coll *mongo.Collection) (cm.Vector, cm.Vector, error) {
	results, err := GetAggregation(coll, GroupMeanStd)
	if err != nil {
		log.Println("Making db aggregation: " + err.Error())
		return cm.Vector{}, cm.Vector{}, err
	}
	convMean, err := ConvertAggResult(results[0]["avg"])
	if err != nil {
		log.Println("Parsing aggregation result: " + err.Error())
		return cm.Vector{}, cm.Vector{}, err
	}
	convStd, err := ConvertAggResult(results[0]["std"])
	if err != nil {
		log.Println("Parsing aggregation result: " + err.Error())
		return cm.Vector{}, cm.Vector{}, err
	}
	return convMean, convStd, nil
}

// UpdateField updates the selected field of the doc.
// Example:
//     filter := bson.D{{"_id", id}}
//     update := bson.D{{"$set", bson.D{{"email", "newemail@example.com"}}}}
func UpdateField(coll *mongo.Collection, filter, update bson.D) error {
	opts := options.Update().SetUpsert(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dbtimeOut)*time.Second)
	defer cancel()
	_, err := coll.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return err
	}
	return nil
}

// SetRecords adds the new documents to the collection
// docs := []interface{}{
//     bson.D{{"name", "Alice"}},
//     bson.D{{"name", "Bob"}},
// }
func SetRecords(coll *mongo.Collection, data []interface{}) error {
	opts := options.InsertMany().SetOrdered(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dbtimeOut)*time.Second)
	defer cancel()
	_, err := coll.InsertMany(ctx, data, opts)
	if err != nil {
		return err
	}
	return nil
}

// DeleteRecords deletes records from specified collection by query
func DeleteRecords(coll *mongo.Collection, query bson.D) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dbtimeOut)*time.Second)
	defer cancel()
	_, err := coll.DeleteMany(ctx, query, nil)
	if err != nil {
		return err
	}
	return nil
}

// GetCursor returns db cursor for specified collection and query
// Example queries:
//     bson.D{{"origId", bson.M{"$in": []int{1, 3}}}}
// 	   bson.D{{"indexer", bson.D{{"$exists", true}}}}
func GetCursor(coll *mongo.Collection, query FindQuery) (*mongo.Cursor, error) {
	opts := options.MergeFindOptions(
		options.Find().SetLimit(int64(query.Limit)),
		options.Find().SetProjection(query.Proj),
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dbtimeOut)*time.Second)
	defer cancel()
	cursor, err := coll.Find(ctx, query.Query, opts)
	if err != nil {
		return nil, err
	}
	return cursor, nil
}

// GetDbRecords get documents from the "main" db collection by field and query (aka `find`)
func GetDbRecords(coll *mongo.Collection, query FindQuery) ([]VectorRecord, error) {
	cursor, err := GetCursor(coll, query)
	if err != nil {
		return nil, err
	}
	var results []VectorRecord
	err = cursor.All(context.Background(), &results)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// GetHelperRecord gets supplementary data from the specified collection
func GetHelperRecord(coll *mongo.Collection, getIndexerObject bool) (HelperRecord, error) {
	proj := bson.M{"indexer": 1, "isBuildDone": 1, "HashCollName": 1}
	if !getIndexerObject {
		proj = bson.M{"indexer": 0}
	}
	cursor, err := GetCursor(
		coll,
		FindQuery{
			Limit: 1,
			Query: bson.D{
				{"indexer", bson.D{{"$exists", true}}},
			},
			Proj: proj,
		},
	)
	if err != nil {
		return HelperRecord{}, err
	}

	var results []HelperRecord
	err = cursor.All(context.Background(), &results)
	if err != nil || len(results) != 1 {
		return HelperRecord{}, err
	}
	return results[0], nil
}

// GetHashesRecords gets records from the specified hashes collection
func GetHashesRecords(coll *mongo.Collection, query FindQuery) ([]HashesRecord, error) {
	cursor, err := GetCursor(coll, query)
	if err != nil {
		return nil, err
	}
	var results []HashesRecord
	err = cursor.All(context.Background(), &results)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// CreateCollection checks if the helper collection exists
// in the db, and creates them if needed
func CreateCollection(dataBase *mongo.Database, collName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dbtimeOut)*time.Second)
	defer cancel()
	err := dataBase.CreateCollection(ctx, collName, nil)
	if err != nil {
		return err
	}
	return nil
}

// DropCollection drops collection on the server
func DropCollection(database *mongo.Database, collectionName string) error {
	coll := database.Collection(collectionName)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dbtimeOut)*time.Second)
	defer cancel()
	err := coll.Drop(ctx)
	if err != nil {
		return err
	}
	return nil
}
