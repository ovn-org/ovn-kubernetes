package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
)

func main() {
	bind_address := fmt.Sprintf(":%s", os.Args[1])

	listener, err := net.Listen("tcp", bind_address)
	if err != nil {
		log.Fatalln(fmt.Errorf("failed listening to %s: %w", bind_address, err))
	}
	defer listener.Close()

	log.Println("Server is running on:", bind_address)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("failed accepting connection: %w", err)
			continue
		}

		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	log.Printf("Handling connection %s\n", conn.RemoteAddr())
	defer func() {
		log.Printf("Closing connection %s\n", conn.RemoteAddr())
		conn.Close()
	}()
	_, err := io.Copy(conn, conn)
	if err != nil {
		log.Printf("failed copying data: %v\n", err)
	}
}
