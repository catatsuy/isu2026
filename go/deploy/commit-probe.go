// Run only as a diagnostic; creates its own small test table on Primary.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	config, err := pgx.ParseConfig("")
	must(err)
	config.Host = os.Getenv("ISUREAL_DB_HOST")
	config.Port = 5432
	config.User = os.Getenv("ISUREAL_DB_USER")
	config.Password = os.Getenv("ISUREAL_DB_PASSWORD")
	config.Database = "isureal"
	db, err := pgx.ConnectConfig(ctx, config)
	must(err)
	defer db.Close(ctx)
	_, err = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS replica_validation_probe (id bigint GENERATED ALWAYS AS IDENTITY, created_at timestamptz DEFAULT clock_timestamp())`)
	must(err)
	values := []float64{}
	for i := 0; i < 30; i++ {
		tx, err := db.Begin(ctx)
		must(err)
		_, err = tx.Exec(ctx, `INSERT INTO replica_validation_probe DEFAULT VALUES`)
		must(err)
		started := time.Now()
		must(tx.Commit(ctx))
		values = append(values, float64(time.Since(started).Microseconds())/1000)
	}
	sort.Float64s(values)
	data, _ := json.Marshal(map[string]any{"n": len(values), "commit_p50_ms": values[14], "commit_p95_ms": values[28], "commit_p99_ms": values[29]})
	fmt.Println(string(data))
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
