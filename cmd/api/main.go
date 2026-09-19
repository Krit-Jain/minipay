package main

import (
	"log"
	"net/http"
	"time"

	"minipay/internal/httpapi"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/orders", httpapi.CreateOrder)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("MiniPay listening on :8080")

	log.Fatal(server.ListenAndServe())
}
