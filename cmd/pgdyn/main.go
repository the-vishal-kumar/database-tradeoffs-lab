// Command pgdyn runs the same orders through PostgreSQL and a DynamoDB
// single-table design: the access pattern both were built for, an ad-hoc
// report, and an access pattern added later.
//
//	docker compose --profile acid up -d --wait postgres dynamodb
//	go run ./cmd/pgdyn
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/pgdyn"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func dur(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
}

func main() {
	n := flag.Int("orders", 200_000, "orders")
	flag.Parse()
	ctx := context.Background()
	pg, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://lab:lab@localhost:55432/lab?sslmode=disable"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pg.Close()
	dy := dynamodb.New(dynamodb.Options{Region: "us-east-1", BaseEndpoint: aws.String(env("DYNAMODB_URL", "http://localhost:58000")),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")})
	e := pgdyn.Env{PG: pg, Dyn: dy, Orders: *n}
	t := time.Now()
	if err := e.Load(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	fmt.Printf("%d orders from 1,000 customers over 2026, loaded into both in %.0f s. PostgreSQL: one table, index on (customer_id, created_at). DynamoDB Local: one table, pk = CUSTOMER#id, sk = ORDER#timestamp#id.\n\n", *n, time.Since(t).Seconds())
	fmt.Println("| Query | Store | p50 | Rows returned | Items read | Read capacity consumed | Note |")
	fmt.Println("|---|---|---:|---:|---:|---:|---|")
	for _, f := range []func(context.Context, int) ([]pgdyn.Result, error){e.Latest, e.AdHoc, e.NewPattern} {
		rs, err := f(ctx, 50)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		for _, r := range rs {
			items, rcu := "—", "—"
			if r.Store == "DynamoDB" {
				items, rcu = fmt.Sprint(r.ItemsRead), fmt.Sprintf("%.1f RCU", r.CapacityUnits)
			}
			fmt.Printf("| %s | %s | %s | %d | %s | %s | %s |\n", r.Query, r.Store, dur(r.P50), r.Rows, items, rcu, r.Note)
		}
	}
	_, _ = pg.Exec(ctx, `DROP SCHEMA IF EXISTS pgdyn CASCADE`)
}
