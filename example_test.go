package xpc_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/pubisu/go-xpc"
)

func ExampleConn_Call() {
	conn, err := xpc.Dial("com.example.helper")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := struct {
		Command string `xpc:"command"`
	}{"status"}
	var status struct {
		Running bool   `xpc:"running"`
		Version string `xpc:"version"`
	}
	if err := conn.Call(ctx, &request, &status); err != nil {
		log.Fatal(err)
	}
	fmt.Println(status.Running, status.Version)
}

func ExampleDialRPC() {
	// A service that names the operation in its "op" entry, takes the
	// operands as entries of their own and reports failures in "error".
	protocol := xpc.RPCProtocol{
		MethodKey: "op",
		Error: func(reply *xpc.Message) error {
			var r struct {
				Error string `xpc:"error"`
			}
			if err := reply.Decode(&r); err != nil {
				return err
			}
			if r.Error != "" {
				return errors.New(r.Error)
			}
			return nil
		},
	}
	client, err := xpc.DialRPC("com.example.arith", protocol)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	type Args struct {
		A int `xpc:"a"`
		B int `xpc:"b"`
	}
	var reply struct {
		Product int `xpc:"product"`
	}
	if err := client.Call("multiply", Args{7, 8}, &reply); err != nil {
		log.Fatal(err)
	}
	fmt.Println(reply.Product)
}
