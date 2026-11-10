// Package pgdyn stores the same orders in PostgreSQL (tables and indexes)
// and in DynamoDB (one table designed around its access patterns), then
// runs the queries a SaaS starts with and the ones it adds later, and
// measures what each costs: latency, and in DynamoDB the items read and
// capacity consumed.
package pgdyn

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Env struct {
	PG     *pgxpool.Pool
	Dyn    *dynamodb.Client
	Orders int // total orders
}

const (
	customers = 1000
	table     = "pgdyn_orders"
)

var statuses = []string{"placed", "paid", "shipped", "delivered", "cancelled"}

type order struct {
	id, customer, total int
	status              string
	at                  time.Time
}

func (e Env) orders() []order {
	r := rand.New(rand.NewPCG(5, 5))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]order, e.Orders)
	for i := range out {
		out[i] = order{id: i + 1, customer: 1 + r.IntN(customers), total: 5 + r.IntN(995),
			status: statuses[r.IntN(len(statuses))], at: base.Add(time.Duration(r.Int64N(int64(300 * 24 * time.Hour))))}
	}
	return out
}

// Load writes the same orders to both stores.
func (e Env) Load(ctx context.Context) error {
	all := e.orders()
	if _, err := e.PG.Exec(ctx, `DROP SCHEMA IF EXISTS pgdyn CASCADE; CREATE SCHEMA pgdyn;
		CREATE TABLE pgdyn.orders (id int PRIMARY KEY, customer_id int NOT NULL, status text NOT NULL,
		                           total int NOT NULL, created_at timestamptz NOT NULL);`); err != nil {
		return err
	}
	rows := make([][]any, len(all))
	for i, o := range all {
		rows[i] = []any{o.id, o.customer, o.status, o.total, o.at}
	}
	if _, err := e.PG.CopyFrom(ctx, []string{"pgdyn", "orders"}, []string{"id", "customer_id", "status", "total", "created_at"},
		pgx.CopyFromRows(rows)); err != nil {
		return err
	}
	if _, err := e.PG.Exec(ctx, `CREATE INDEX ON pgdyn.orders (customer_id, created_at DESC)`); err != nil {
		return err
	}
	if _, err := e.PG.Exec(ctx, `VACUUM ANALYZE pgdyn.orders`); err != nil { // VACUUM can't share a multi-statement query
		return err
	}

	// DynamoDB: designed for "a customer's orders, newest first".
	_, _ = e.Dyn.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	if _, err := e.Dyn.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(table),
		AttributeDefinitions: []ddb.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: ddb.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: ddb.ScalarAttributeTypeS}},
		KeySchema: []ddb.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: ddb.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: ddb.KeyTypeRange}},
		BillingMode: ddb.BillingModePayPerRequest,
	}); err != nil {
		return err
	}
	for i := 0; i < len(all); i += 25 {
		var reqs []ddb.WriteRequest
		for _, o := range all[i:min(i+25, len(all))] {
			reqs = append(reqs, ddb.WriteRequest{PutRequest: &ddb.PutRequest{Item: item(o)}})
		}
		for len(reqs) > 0 {
			out, err := e.Dyn.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]ddb.WriteRequest{table: reqs}})
			if err != nil {
				return err
			}
			reqs = out.UnprocessedItems[table]
		}
	}
	return nil
}

func item(o order) map[string]ddb.AttributeValue {
	return map[string]ddb.AttributeValue{
		"pk":       &ddb.AttributeValueMemberS{Value: fmt.Sprintf("CUSTOMER#%d", o.customer)},
		"sk":       &ddb.AttributeValueMemberS{Value: fmt.Sprintf("ORDER#%s#%07d", o.at.Format(time.RFC3339), o.id)},
		"status":   &ddb.AttributeValueMemberS{Value: o.status},
		"total":    &ddb.AttributeValueMemberN{Value: strconv.Itoa(o.total)},
		"created":  &ddb.AttributeValueMemberS{Value: o.at.Format(time.RFC3339)},
		"order_id": &ddb.AttributeValueMemberN{Value: strconv.Itoa(o.id)},
	}
}

// Result: one query in one store.
type Result struct {
	Query, Store  string
	P50           time.Duration
	Rows          int     // rows/items returned
	ItemsRead     int     // DynamoDB: items the request read (ScannedCount)
	CapacityUnits float64 // DynamoDB: read capacity consumed
	Note          string
}

func pct(d []time.Duration) time.Duration {
	slices.Sort(d)
	return d[len(d)/2]
}

func (e Env) timePG(ctx context.Context, name string, runs int, sql string, args func() []any) (Result, error) {
	var lat []time.Duration
	n := 0
	for range runs {
		t := time.Now()
		rows, err := e.PG.Query(ctx, sql, args()...)
		if err != nil {
			return Result{}, err
		}
		n = 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			return Result{}, err
		}
		lat = append(lat, time.Since(t))
	}
	return Result{Query: name, Store: "PostgreSQL", P50: pct(lat), Rows: n}, nil
}

// Customer's latest 20 orders: the access pattern both were designed for.
func (e Env) Latest(ctx context.Context, runs int) ([]Result, error) {
	r := rand.New(rand.NewPCG(1, 2))
	pg, err := e.timePG(ctx, "a customer's 20 latest orders", runs,
		`SELECT id, status, total, created_at FROM pgdyn.orders WHERE customer_id = $1 ORDER BY created_at DESC LIMIT 20`,
		func() []any { return []any{1 + r.IntN(customers)} })
	if err != nil {
		return nil, err
	}
	var lat []time.Duration
	res := Result{Query: pg.Query, Store: "DynamoDB"}
	for range runs {
		t := time.Now()
		out, err := e.Dyn.Query(ctx, &dynamodb.QueryInput{TableName: aws.String(table),
			KeyConditionExpression: aws.String("pk = :c AND begins_with(sk, :o)"),
			ExpressionAttributeValues: map[string]ddb.AttributeValue{
				":c": &ddb.AttributeValueMemberS{Value: fmt.Sprintf("CUSTOMER#%d", 1+r.IntN(customers))},
				":o": &ddb.AttributeValueMemberS{Value: "ORDER#"}},
			ScanIndexForward: aws.Bool(false), Limit: aws.Int32(20), ReturnConsumedCapacity: ddb.ReturnConsumedCapacityTotal})
		if err != nil {
			return nil, err
		}
		lat = append(lat, time.Since(t))
		res.Rows, res.ItemsRead = len(out.Items), int(out.ScannedCount)
		if out.ConsumedCapacity != nil {
			res.CapacityUnits = aws.ToFloat64(out.ConsumedCapacity.CapacityUnits)
		}
	}
	res.P50 = pct(lat)
	return []Result{pg, res}, nil
}

// An ad-hoc report nobody planned: paid orders over 900 in March, across
// all customers. PostgreSQL plans a query; DynamoDB has to scan.
func (e Env) AdHoc(ctx context.Context, runs int) ([]Result, error) {
	pg, err := e.timePG(ctx, "report: paid orders over 900 in March, all customers", runs,
		`SELECT id, customer_id, total FROM pgdyn.orders WHERE status = 'paid' AND total > 900
		 AND created_at >= '2026-03-01' AND created_at < '2026-04-01'`, func() []any { return nil })
	if err != nil {
		return nil, err
	}
	pg.Note = "no index for it: a sequential scan, planned automatically"
	res := Result{Query: pg.Query, Store: "DynamoDB", Note: "Scan of the whole table, filter applied after reading"}
	var lat []time.Duration
	for range max(1, runs/5) {
		t := time.Now()
		var start map[string]ddb.AttributeValue
		res.Rows, res.ItemsRead, res.CapacityUnits = 0, 0, 0
		for {
			out, err := e.Dyn.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(table),
				FilterExpression:         aws.String("#s = :s AND #t > :t AND created BETWEEN :a AND :b"),
				ExpressionAttributeNames: map[string]string{"#s": "status", "#t": "total"},
				ExpressionAttributeValues: map[string]ddb.AttributeValue{
					":s": &ddb.AttributeValueMemberS{Value: "paid"}, ":t": &ddb.AttributeValueMemberN{Value: "900"},
					":a": &ddb.AttributeValueMemberS{Value: "2026-03-01"}, ":b": &ddb.AttributeValueMemberS{Value: "2026-04-01"}},
				ExclusiveStartKey: start, ReturnConsumedCapacity: ddb.ReturnConsumedCapacityTotal})
			if err != nil {
				return nil, err
			}
			res.Rows += len(out.Items)
			res.ItemsRead += int(out.ScannedCount)
			if out.ConsumedCapacity != nil {
				res.CapacityUnits += aws.ToFloat64(out.ConsumedCapacity.CapacityUnits)
			}
			if len(out.LastEvaluatedKey) == 0 {
				break
			}
			start = out.LastEvaluatedKey
		}
		lat = append(lat, time.Since(t))
	}
	res.P50 = pct(lat)
	return []Result{pg, res}, nil
}

// NewPattern: "all orders with status X, newest first" was not designed
// for. PostgreSQL: one CREATE INDEX. DynamoDB: a global secondary index,
// built by backfilling every item; then a Query.
func (e Env) NewPattern(ctx context.Context, runs int) ([]Result, error) {
	t := time.Now()
	if _, err := e.PG.Exec(ctx, `CREATE INDEX ON pgdyn.orders (status, created_at DESC)`); err != nil {
		return nil, err
	}
	pgBuild := time.Since(t)
	pg, err := e.timePG(ctx, "latest 20 orders with status 'placed', all customers", runs,
		`SELECT id, customer_id, total FROM pgdyn.orders WHERE status = 'placed' ORDER BY created_at DESC LIMIT 20`,
		func() []any { return nil })
	if err != nil {
		return nil, err
	}
	pg.Note = fmt.Sprintf("after `CREATE INDEX` (%s)", round(pgBuild))

	t = time.Now()
	if _, err := e.Dyn.UpdateTable(ctx, &dynamodb.UpdateTableInput{TableName: aws.String(table),
		AttributeDefinitions: []ddb.AttributeDefinition{
			{AttributeName: aws.String("status"), AttributeType: ddb.ScalarAttributeTypeS},
			{AttributeName: aws.String("created"), AttributeType: ddb.ScalarAttributeTypeS}},
		GlobalSecondaryIndexUpdates: []ddb.GlobalSecondaryIndexUpdate{{Create: &ddb.CreateGlobalSecondaryIndexAction{
			IndexName:  aws.String("by_status"),
			KeySchema:  []ddb.KeySchemaElement{{AttributeName: aws.String("status"), KeyType: ddb.KeyTypeHash}, {AttributeName: aws.String("created"), KeyType: ddb.KeyTypeRange}},
			Projection: &ddb.Projection{ProjectionType: ddb.ProjectionTypeAll},
		}}},
	}); err != nil {
		return nil, err
	}
	for i := 0; i < 600; i++ { // wait for the index to be built (backfilled)
		out, err := e.Dyn.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
		if err != nil {
			return nil, err
		}
		if len(out.Table.GlobalSecondaryIndexes) > 0 && out.Table.GlobalSecondaryIndexes[0].IndexStatus == ddb.IndexStatusActive {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	dynBuild := time.Since(t)
	res := Result{Query: pg.Query, Store: "DynamoDB", Note: fmt.Sprintf("after adding a GSI (status, created): %s locally; in AWS, a backfill of every item", round(dynBuild))}
	var lat []time.Duration
	for range runs {
		t := time.Now()
		out, err := e.Dyn.Query(ctx, &dynamodb.QueryInput{TableName: aws.String(table), IndexName: aws.String("by_status"),
			KeyConditionExpression:    aws.String("#s = :s"),
			ExpressionAttributeNames:  map[string]string{"#s": "status"},
			ExpressionAttributeValues: map[string]ddb.AttributeValue{":s": &ddb.AttributeValueMemberS{Value: "placed"}},
			ScanIndexForward:          aws.Bool(false), Limit: aws.Int32(20), ReturnConsumedCapacity: ddb.ReturnConsumedCapacityTotal})
		if err != nil {
			return nil, err
		}
		lat = append(lat, time.Since(t))
		res.Rows, res.ItemsRead = len(out.Items), int(out.ScannedCount)
		if out.ConsumedCapacity != nil {
			res.CapacityUnits = aws.ToFloat64(out.ConsumedCapacity.CapacityUnits)
		}
	}
	res.P50 = pct(lat)
	return []Result{pg, res}, nil
}

func round(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}
