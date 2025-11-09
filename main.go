package main

import (
	"context"
	"log"
	"net/http"
	"os"

	httpapi "promote/internal/http"
	"promote/internal/storage"
	"promote/internal/wa"
)

func main() {
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "file:promote.db?_foreign_keys=on"
	}

	store, err := storage.Open(dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	manager, err := wa.NewManager(ctx, dsn, store)
	if err != nil {
		log.Fatal(err)
	}

	router := httpapi.NewRouter(store, manager)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Println("HTTP listening on :" + port)
	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatal(err)
	}
}
