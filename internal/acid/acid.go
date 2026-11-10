// Package acid moves money between 100 accounts in PostgreSQL, MongoDB and
// DynamoDB (Local), with and without transactions, crashing 2% of
// transfers half-way, while a reader keeps adding up every balance. What
// each letter of ACID buys shows up as numbers: the total drifting
// (atomicity), balances going negative (consistency), the reader seeing a
// total that never existed (isolation).
package acid

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

const (
	Accounts     = 100
	StartBalance = 1000
	Total        = Accounts * StartBalance
)

var errCrash = errors.New("crash")

// Store moves money one way. Transfer returns ok=false when the source
// lacks the funds; retries counts conflicts it retried.
type Store interface {
	Name() string
	Setup(ctx context.Context) error
	Transfer(ctx context.Context, from, to, amount int, crash bool) (ok bool, retries int, err error)
	// ReadTotal adds up every balance: with snapshot, in one consistent
	// snapshot where the store offers one; otherwise the plain way.
	ReadTotal(ctx context.Context, snapshot bool) (int64, error)
	Negative(ctx context.Context) (int, error)
}

// ---- PostgreSQL ----------------------------------------------------------------

type PG struct {
	DB *pgxpool.Pool
	Tx bool
}

func (p PG) Name() string {
	if p.Tx {
		return "PostgreSQL, one transaction"
	}
	return "PostgreSQL, two separate UPDATEs"
}

func (p PG) Setup(ctx context.Context) error {
	_, err := p.DB.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS acid CASCADE; CREATE SCHEMA acid;
		CREATE TABLE acid.accounts (id int PRIMARY KEY, balance int NOT NULL);
		INSERT INTO acid.accounts SELECT g, %d FROM generate_series(1, %d) g;`, StartBalance, Accounts))
	return err
}

func (p PG) Transfer(ctx context.Context, from, to, amount int, crash bool) (bool, int, error) {
	debit := `UPDATE acid.accounts SET balance = balance - $2 WHERE id = $1 AND balance >= $2`
	credit := `UPDATE acid.accounts SET balance = balance + $2 WHERE id = $1`
	if !p.Tx {
		tag, err := p.DB.Exec(ctx, debit, from, amount)
		if err != nil || tag.RowsAffected() == 0 {
			return false, 0, err
		}
		if crash {
			return false, 0, errCrash // the process dies: the debit stays
		}
		_, err = p.DB.Exec(ctx, credit, to, amount)
		return err == nil, 0, err
	}
	// Two transfers in opposite directions lock the same two rows in
	// opposite orders and can deadlock; PostgreSQL aborts one (40P01), and
	// it's retried. (Locking the lower id first would avoid it.)
	for retries := 0; ; retries++ {
		ok := false
		err := pgx.BeginFunc(ctx, p.DB, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, debit, from, amount)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			if crash {
				return errCrash // the transaction never commits: rolled back
			}
			if _, err := tx.Exec(ctx, credit, to, amount); err != nil {
				return err
			}
			ok = true
			return nil
		})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && (pe.Code == "40P01" || pe.Code == "40001") && retries < 50 {
			continue
		}
		return ok, retries, err
	}
}

// One SQL statement always reads one snapshot, so both reads are the same.
func (p PG) ReadTotal(ctx context.Context, _ bool) (int64, error) {
	var t int64
	err := p.DB.QueryRow(ctx, `SELECT sum(balance) FROM acid.accounts`).Scan(&t)
	return t, err
}

func (p PG) Negative(ctx context.Context) (int, error) {
	var n int
	err := p.DB.QueryRow(ctx, `SELECT count(*) FROM acid.accounts WHERE balance < 0`).Scan(&n)
	return n, err
}

// ---- MongoDB -------------------------------------------------------------------

type Mongo struct {
	Client *mongo.Client
	Tx     bool
}

func (m Mongo) Name() string {
	if m.Tx {
		return "MongoDB, multi-document transaction"
	}
	return "MongoDB, two separate updateOne calls"
}

func (m Mongo) coll() *mongo.Collection { return m.Client.Database("acid").Collection("accounts") }

func (m Mongo) Setup(ctx context.Context) error {
	c := m.coll()
	if err := c.Drop(ctx); err != nil {
		return err
	}
	docs := make([]any, Accounts)
	for i := range docs {
		docs[i] = bson.D{{Key: "_id", Value: i + 1}, {Key: "balance", Value: StartBalance}}
	}
	_, err := c.InsertMany(ctx, docs)
	return err
}

func (m Mongo) Transfer(ctx context.Context, from, to, amount int, crash bool) (bool, int, error) {
	c := m.coll()
	steps := func(ctx context.Context) (bool, error) {
		r, err := c.UpdateOne(ctx, bson.D{{Key: "_id", Value: from}, {Key: "balance", Value: bson.D{{Key: "$gte", Value: amount}}}},
			bson.D{{Key: "$inc", Value: bson.D{{Key: "balance", Value: -amount}}}})
		if err != nil || r.MatchedCount == 0 {
			return false, err
		}
		if crash {
			return false, errCrash
		}
		_, err = c.UpdateOne(ctx, bson.D{{Key: "_id", Value: to}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "balance", Value: amount}}}})
		return err == nil, err
	}
	if !m.Tx {
		ok, err := steps(ctx)
		return ok, 0, err
	}
	sess, err := m.Client.StartSession()
	if err != nil {
		return false, 0, err
	}
	defer sess.EndSession(ctx)
	attempts := 0
	var ok bool
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		attempts++ // WithTransaction retries write conflicts itself
		var err error
		ok, err = steps(ctx)
		return nil, err
	})
	return ok, attempts - 1, err
}

func (m Mongo) ReadTotal(ctx context.Context, snapshot bool) (int64, error) {
	sum := func(ctx context.Context) (int64, error) {
		// Batches of 10, as on any collection bigger than one batch: each
		// batch is read at its own moment.
		cur, err := m.coll().Find(ctx, bson.D{}, options.Find().SetBatchSize(10))
		if err != nil {
			return 0, err
		}
		var docs []struct {
			Balance int64 `bson:"balance"`
		}
		if err := cur.All(ctx, &docs); err != nil {
			return 0, err
		}
		var t int64
		for _, d := range docs {
			t += d.Balance
		}
		return t, nil
	}
	if !snapshot {
		return sum(ctx) // documents read one after another, each at its own moment
	}
	sess, err := m.Client.StartSession()
	if err != nil {
		return 0, err
	}
	defer sess.EndSession(ctx)
	var t int64
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		var err error
		t, err = sum(ctx)
		return nil, err
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()))
	return t, err
}

func (m Mongo) Negative(ctx context.Context) (int, error) {
	n, err := m.coll().CountDocuments(ctx, bson.D{{Key: "balance", Value: bson.D{{Key: "$lt", Value: 0}}}})
	return int(n), err
}

// ---- DynamoDB --------------------------------------------------------------------

type Dynamo struct {
	DB *dynamodb.Client
	Tx bool
}

const table = "acid_accounts"

func (d Dynamo) Name() string {
	if d.Tx {
		return "DynamoDB, TransactWriteItems"
	}
	return "DynamoDB, two separate UpdateItem calls"
}

func idKey(id int) map[string]ddb.AttributeValue {
	return map[string]ddb.AttributeValue{"id": &ddb.AttributeValueMemberN{Value: strconv.Itoa(id)}}
}

func (d Dynamo) Setup(ctx context.Context) error {
	_, _ = d.DB.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	if _, err := d.DB.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(table),
		AttributeDefinitions: []ddb.AttributeDefinition{{AttributeName: aws.String("id"), AttributeType: ddb.ScalarAttributeTypeN}},
		KeySchema:            []ddb.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: ddb.KeyTypeHash}},
		BillingMode:          ddb.BillingModePayPerRequest,
	}); err != nil {
		return err
	}
	for i := 1; i <= Accounts; i++ {
		item := idKey(i)
		item["balance"] = &ddb.AttributeValueMemberN{Value: strconv.Itoa(StartBalance)}
		if _, err := d.DB.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: item}); err != nil {
			return err
		}
	}
	return nil
}

func amountAV(a int) map[string]ddb.AttributeValue {
	return map[string]ddb.AttributeValue{":a": &ddb.AttributeValueMemberN{Value: strconv.Itoa(a)}}
}

func (d Dynamo) Transfer(ctx context.Context, from, to, amount int, crash bool) (bool, int, error) {
	if !d.Tx {
		_, err := d.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(table), Key: idKey(from),
			UpdateExpression:          aws.String("SET balance = balance - :a"),
			ConditionExpression:       aws.String("balance >= :a"),
			ExpressionAttributeValues: amountAV(amount),
		})
		var ccf *ddb.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return false, 0, nil
		}
		if err != nil {
			return false, 0, err
		}
		if crash {
			return false, 0, errCrash
		}
		_, err = d.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(table), Key: idKey(to),
			UpdateExpression:          aws.String("SET balance = balance + :a"),
			ExpressionAttributeValues: amountAV(amount),
		})
		return err == nil, 0, err
	}
	if crash {
		return false, 0, errCrash // the request was never sent: nothing happened
	}
	for retries := 0; retries < 50; retries++ {
		_, err := d.DB.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []ddb.TransactWriteItem{
			{Update: &ddb.Update{TableName: aws.String(table), Key: idKey(from),
				UpdateExpression: aws.String("SET balance = balance - :a"), ConditionExpression: aws.String("balance >= :a"),
				ExpressionAttributeValues: amountAV(amount)}},
			{Update: &ddb.Update{TableName: aws.String(table), Key: idKey(to),
				UpdateExpression: aws.String("SET balance = balance + :a"), ExpressionAttributeValues: amountAV(amount)}},
		}})
		var tce *ddb.TransactionCanceledException
		if errors.As(err, &tce) {
			for _, r := range tce.CancellationReasons {
				if aws.ToString(r.Code) == "ConditionalCheckFailed" {
					return false, retries, nil // insufficient funds
				}
			}
			time.Sleep(time.Duration(1+rand.IntN(5)) * time.Millisecond) // a conflict: retry
			continue
		}
		return err == nil, retries, err
	}
	return false, 50, errors.New("gave up after 50 conflicts")
}

func (d Dynamo) ReadTotal(ctx context.Context, snapshot bool) (int64, error) {
	sum := func(items []map[string]ddb.AttributeValue) int64 {
		var t int64
		for _, it := range items {
			if n, ok := it["balance"].(*ddb.AttributeValueMemberN); ok {
				v, _ := strconv.ParseInt(n.Value, 10, 64)
				t += v
			}
		}
		return t
	}
	if !snapshot {
		// Pages of 10 items, as on any table bigger than one page (1 MB).
		var items []map[string]ddb.AttributeValue
		var start map[string]ddb.AttributeValue
		for {
			out, err := d.DB.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(table), ConsistentRead: aws.Bool(true),
				Limit: aws.Int32(10), ExclusiveStartKey: start})
			if err != nil {
				return 0, err
			}
			items = append(items, out.Items...)
			if len(out.LastEvaluatedKey) == 0 {
				break
			}
			start = out.LastEvaluatedKey
		}
		return sum(items), nil
	}
	gets := make([]ddb.TransactGetItem, Accounts) // up to 100 items per transaction
	for i := range gets {
		gets[i] = ddb.TransactGetItem{Get: &ddb.Get{TableName: aws.String(table), Key: idKey(i + 1)}}
	}
	out, err := d.DB.TransactGetItems(ctx, &dynamodb.TransactGetItemsInput{TransactItems: gets})
	if err != nil {
		var tce *ddb.TransactionCanceledException
		if errors.As(err, &tce) {
			return -1, nil // conflicted with a write in progress: no answer, rather than a wrong one
		}
		return 0, err
	}
	items := make([]map[string]ddb.AttributeValue, len(out.Responses))
	for i, r := range out.Responses {
		items[i] = r.Item
	}
	return sum(items), nil
}

func (d Dynamo) Negative(ctx context.Context) (int, error) {
	out, err := d.DB.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(table), ConsistentRead: aws.Bool(true),
		FilterExpression: aws.String("balance < :z"), ExpressionAttributeValues: map[string]ddb.AttributeValue{":z": &ddb.AttributeValueMemberN{Value: "0"}}})
	if err != nil {
		return 0, err
	}
	return int(out.Count), nil
}

// ---- the run -----------------------------------------------------------------------

type Result struct {
	Store                          string
	Transfers, Declined, Crashed   int
	Retries                        int
	FinalTotal                     int64
	Negative                       int
	Reads, WrongReads, Unavailable int // reader: totals read, totals != Total, reads refused (conflict)
	Took                           time.Duration
}

// Run: workers × perWorker random transfers of 1-100; crashP of them crash
// half-way; a reader sums all balances continuously.
func Run(ctx context.Context, s Store, workers, perWorker int, crashP float64, snapshotReads bool, seed uint64) (Result, error) {
	if err := s.Setup(ctx); err != nil {
		return Result{}, err
	}
	r := Result{Store: s.Name()}
	var ok, declined, crashed, retries atomic.Int64
	var firstErr atomic.Value
	var wg sync.WaitGroup
	done := make(chan struct{})
	var reads, wrong, unavailable int
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-done:
				return
			default:
			}
			t, err := s.ReadTotal(ctx, snapshotReads)
			if err != nil {
				continue
			}
			reads++
			switch {
			case t < 0:
				unavailable++
			case t != Total:
				wrong++
			}
		}
	}()
	start := time.Now()
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w)))
			for range perWorker {
				from := 1 + rng.IntN(Accounts)
				to := 1 + rng.IntN(Accounts-1)
				if to >= from {
					to++
				}
				good, n, err := s.Transfer(ctx, from, to, 1+rng.IntN(100), rng.Float64() < crashP)
				retries.Add(int64(n))
				switch {
				case errors.Is(err, errCrash):
					crashed.Add(1)
				case err != nil:
					firstErr.CompareAndSwap(nil, err)
					return
				case good:
					ok.Add(1)
				default:
					declined.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	r.Took = time.Since(start)
	close(done)
	<-readerDone
	if err, _ := firstErr.Load().(error); err != nil {
		return r, fmt.Errorf("%s: %w", s.Name(), err)
	}
	r.Transfers, r.Declined, r.Crashed, r.Retries = int(ok.Load()), int(declined.Load()), int(crashed.Load()), int(retries.Load())
	r.Reads, r.WrongReads, r.Unavailable = reads, wrong, unavailable
	var err error
	if r.FinalTotal, err = s.ReadTotal(ctx, true); err != nil {
		return r, err
	}
	r.Negative, err = s.Negative(ctx)
	return r, err
}
