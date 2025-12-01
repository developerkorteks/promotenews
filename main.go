package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"promote/internal/autojoin"
	httpapi "promote/internal/http"
	"promote/internal/scheduler"
	"promote/internal/sender"
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

	// Inisialisasi auto-join handler
	autoJoiner := autojoin.New(store, manager)
	manager.AddMessageHandler(autoJoiner.HandleMessage)
	log.Println("Auto-join handler registered")

	// Inisialisasi pengirim dan scheduler anti-spam (aktif otomatis dengan jendela aman WIB).
	snd := sender.New(store, manager)
	sched := scheduler.New(store, manager, snd)
	sched.Start(ctx)

	router := httpapi.NewRouter(store, manager, autoJoiner)

	port := os.Getenv("PORT")
	if port == "" {
		port = "9724"
	}
	log.Println("HTTP listening on :" + port)
	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatal(err)
	}
}
