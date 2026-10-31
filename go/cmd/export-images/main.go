package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	isureal "example.com/isureal-company/isureal"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

func main() {
	config := isureal.ConfigFromEnv()
	root := flag.String("dir", config.ImageFileDir, "destination image directory")
	since := flag.String("since", "", "export created_at >= RFC3339 timestamp (delta export)")
	pause := flag.Duration("pause", 10*time.Millisecond, "pause every 1000 images")
	flag.Parse()
	if !filepath.IsAbs(*root) {
		log.Fatal("destination must be absolute")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	db, err := sqlx.Open("pgx", config.DatabaseDSN())
	if err != nil {
		log.Fatal("database open failed")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	query := "SELECT id, data FROM image_objects WHERE data IS NOT NULL"
	var args []any
	if *since != "" {
		timestamp, err := time.Parse(time.RFC3339Nano, *since)
		if err != nil {
			log.Fatal("invalid --since timestamp")
		}
		query += " AND created_at >= $1"
		args = append(args, timestamp)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Fatal("export query failed")
	}
	defer rows.Close()
	count := 0
	var size int64
	started := time.Now().UTC()
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			log.Fatal("image scan failed")
		}
		if err := isureal.WriteImageFile(*root, id, data, false); err != nil {
			log.Fatalf("image file write failed: %v", err)
		}
		count++
		size += int64(len(data))
		if count%10000 == 0 {
			log.Printf("exported=%d bytes=%d elapsed=%s", count, size, time.Since(started).Round(time.Second))
		}
		if count%1000 == 0 {
			select {
			case <-ctx.Done():
				log.Fatal("export canceled")
			case <-time.After(*pause):
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatal("export stream failed")
	}
	if err := exec.CommandContext(ctx, "sync").Run(); err != nil {
		log.Fatal("filesystem synchronization failed")
	}
	marker := fmt.Sprintf("started=%s\nfinished=%s\nimages=%d\nbytes=%d\nsince=%s\n", started.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), count, size, *since)
	name := ".export-complete"
	if *since != "" {
		name = ".delta-complete"
	}
	if err := os.WriteFile(filepath.Join(*root, name), []byte(marker), 0644); err != nil {
		log.Fatal("export marker failed")
	}
	log.Printf("COMPLETE images=%d bytes=%d", count, size)
}
