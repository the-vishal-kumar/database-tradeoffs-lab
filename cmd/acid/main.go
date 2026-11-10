// Command acid moves money between 100 accounts in PostgreSQL, MongoDB and
// DynamoDB Local, with and without transactions, crashing 2% of transfers
// half-way while a reader keeps adding up every balance.
//
//	docker compose --profile acid up -d --wait postgres mongors dynamodb
//	go run ./cmd/acid
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/acid"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	pg, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://lab:lab@localhost:55432/lab?sslmode=disable"))
	if err != nil {
		return err
	}
	defer pg.Close()
	mc, err := mongo.Connect(options.Client().ApplyURI(env("MONGO_RS_URL", "mongodb://localhost:57018/?directConnection=true")))
	if err != nil {
		return err
	}
	defer mc.Disconnect(ctx)
	dy := dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(env("DYNAMODB_URL", "http://localhost:58000")),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""), // DynamoDB Local ignores them
	})
	const workers, per, crash = 8, 1000, 0.02
	fmt.Printf("100 accounts × 1,000 = 100,000 in total. %d workers make %d random transfers of 1–100 each; %.0f%% of transfers crash half-way. A reader adds up all balances, over and over, while they run.\n\n", workers, per, crash*100)
	fmt.Println("| Store | Transfers | Declined (no funds) | Crashed | Conflicts retried | Final total | Negative balances |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|")
	stores := []acid.Store{
		acid.PG{DB: pg}, acid.PG{DB: pg, Tx: true},
		acid.Mongo{Client: mc}, acid.Mongo{Client: mc, Tx: true},
		acid.Dynamo{DB: dy}, acid.Dynamo{DB: dy, Tx: true},
	}
	for _, s := range stores {
		r, err := acid.Run(ctx, s, workers, per, crash, false, 1)
		if err != nil {
			return err
		}
		fmt.Printf("| %s | %d | %d | %d | %d | **%d** | %d |\n", r.Store, r.Transfers, r.Declined, r.Crashed, r.Retries, r.FinalTotal, r.Negative)
	}
	// Isolation, without crashes: no money is lost, so any total other than
	// 100,000 is a transfer seen half-done.
	fmt.Printf("\n### What a reader sees while transfers run (no crashes, %d transfers)\n\n", workers*per/2)
	fmt.Println("| Writers | Reader | Totals read | Totals that weren't 100,000 |")
	fmt.Println("|---|---|---:|---:|")
	readers := map[string][2]string{
		"PostgreSQL": {"`SELECT sum(balance)` (one statement = one snapshot)", ""},
		"MongoDB":    {"`find()` over all accounts, in batches of 10", "`find()` in a transaction with `readConcern: snapshot`"},
		"DynamoDB":   {"`Scan` with `ConsistentRead`, pages of 10", "`TransactGetItems` (all 100 items)"},
	}
	for _, s := range stores {
		name := s.Name()
		db := name[:strings.Index(name, ",")]
		for i, snap := range []bool{false, true} {
			label := readers[db][i]
			if label == "" {
				continue
			}
			iso, err := acid.Run(ctx, s, workers, per/2, 0, snap, 2)
			if err != nil {
				return err
			}
			extra := ""
			if iso.Unavailable > 0 {
				extra = fmt.Sprintf(" (+%d refused: conflict)", iso.Unavailable)
			}
			fmt.Printf("| %s | %s | %d | **%d**%s |\n", name, label, iso.Reads, iso.WrongReads, extra)
		}
	}
	return nil
}
