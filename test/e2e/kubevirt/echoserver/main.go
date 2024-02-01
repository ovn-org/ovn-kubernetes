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
		panic(fmt.Errorf("failed listening to %s: %w", bind_address, err))
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			panic(fmt.Errorf("failed accepting connection: %w", err))
		}

		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	log.Printf("Handling connection %s\n", conn.RemoteAddr())
	defer func() {
		log.Printf("Clossing connection %s\n", conn.RemoteAddr())
		conn.Close()
	}()
	for {
		receivedMsg := make([]byte, 1024)
		_, err := conn.Read(receivedMsg)
		if err != nil {
			if err != nil {
				if err != io.EOF {
					log.Printf("failed reading data: %v\n", err)
				}
				return
			}
		}
		conn.Write(receivedMsg)
	}
}
