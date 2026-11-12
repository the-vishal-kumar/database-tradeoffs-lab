package acid

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Needs PostgreSQL, the MongoDB replica set and DynamoDB Local (compose
// profile "acid"): `make acid-test`.
func TestACID(t *testing.T) {
	pgURL, mURL, dURL := os.Getenv("DATABASE_URL"), os.Getenv("MONGO_RS_URL"), os.Getenv("DYNAMODB_URL")
	if pgURL == "" || mURL == "" || dURL == "" {
		t.Skip("DATABASE_URL, MONGO_RS_URL and DYNAMODB_URL not set; run `make acid-test`")
	}
	ctx := context.Background()
	pg, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	t.Cleanup(func() { _, _ = pg.Exec(context.Background(), `DROP SCHEMA IF EXISTS acid CASCADE`) })
	mc, err := mongo.Connect(options.Client().ApplyURI(mURL))
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Disconnect(ctx)
	// A short timeout: without one, `make test` (which sets MONGO_RS_URL but
	// doesn't start the replica set) waited 30 s for server selection
	// before skipping.
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := mc.Ping(pctx, nil); err != nil {
		t.Skipf("MongoDB replica set not reachable (start it with `make acid-test`): %v", err)
	}
	dy := dynamodb.New(dynamodb.Options{Region: "us-east-1", BaseEndpoint: aws.String(dURL),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")})

	pairs := [][2]Store{
		{PG{DB: pg}, PG{DB: pg, Tx: true}},
		{Mongo{Client: mc}, Mongo{Client: mc, Tx: true}},
		{Dynamo{DB: dy}, Dynamo{DB: dy, Tx: true}},
	}
	for _, p := range pairs {
		plain, err := Run(ctx, p[0], 4, 150, 0.05, false, 1)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := Run(ctx, p[1], 4, 150, 0.05, false, 1)
		if err != nil {
			t.Fatal(err)
		}
		if plain.Crashed == 0 || plain.FinalTotal >= Total {
			t.Errorf("%s: crashes should lose money: %+v", plain.Store, plain)
		}
		if tx.FinalTotal != Total || tx.Negative != 0 {
			t.Errorf("%s: should keep the total and never go negative: %+v", tx.Store, tx)
		}
	}
	// Isolation: transactional writers, a paged plain read vs a snapshot read.
	for _, s := range []Store{Mongo{Client: mc, Tx: true}, Dynamo{DB: dy, Tx: true}} {
		plain, err := Run(ctx, s, 4, 300, 0, false, 2)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := Run(ctx, s, 4, 300, 0, true, 2)
		if err != nil {
			t.Fatal(err)
		}
		if plain.WrongReads == 0 || snap.WrongReads != 0 {
			t.Errorf("%s: a paged read should see transfers half-done (%d of %d); a snapshot read never (%d of %d)",
				s.Name(), plain.WrongReads, plain.Reads, snap.WrongReads, snap.Reads)
		}
	}
}
