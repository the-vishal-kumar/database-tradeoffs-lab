package pgdyn

import (
	"context"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Needs DynamoDB Local (compose profile "acid"): `make pgdyn-test`.
func TestAccessPatterns(t *testing.T) {
	pgURL, dURL := os.Getenv("DATABASE_URL"), os.Getenv("DYNAMODB_URL")
	if pgURL == "" || dURL == "" {
		t.Skip("DATABASE_URL and DYNAMODB_URL not set; run `make pgdyn-test`")
	}
	ctx := context.Background()
	pg, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	t.Cleanup(func() { _, _ = pg.Exec(context.Background(), `DROP SCHEMA IF EXISTS pgdyn CASCADE`) })
	dy := dynamodb.New(dynamodb.Options{Region: "us-east-1", BaseEndpoint: aws.String(dURL),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")})
	if _, err := dy.ListTables(ctx, &dynamodb.ListTablesInput{}); err != nil {
		t.Skipf("DynamoDB Local not reachable: %v", err)
	}
	e := Env{PG: pg, Dyn: dy, Orders: 5000}
	if err := e.Load(ctx); err != nil {
		t.Fatal(err)
	}
	l, err := e.Latest(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.AdHoc(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	// The designed query reads only what it returns (a few orders per
	// customer at this size); the ad-hoc report reads the whole table.
	if l[1].ItemsRead != l[1].Rows || l[1].Rows == 0 || l[1].Rows > 20 || a[1].ItemsRead != e.Orders {
		t.Errorf("designed query should read only what it returns, ad-hoc report the whole table: %+v, %+v", l[1], a[1])
	}
	if a[0].Rows != a[1].Rows {
		t.Errorf("both stores should find the same report rows: %d vs %d", a[0].Rows, a[1].Rows)
	}
	n, err := e.NewPattern(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n[1].ItemsRead != 20 || n[0].Rows != 20 {
		t.Errorf("after the new index, both should answer with 20 rows read: %+v", n)
	}
}
