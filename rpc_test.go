// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Tamir Duberstein (tamird@gmail.com)

package rpcbench

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/rpc"
	"strings"
	"testing"

	"github.com/gogo/protobuf/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var clientTLSConfig *tls.Config

func init() {
	grpc.EnableTracing = false

	bs, err := ioutil.ReadFile("rootCA.pem")
	if err != nil {
		log.Fatalf("err: %v", err)
	}

	cp := x509.NewCertPool()
	cp.AppendCertsFromPEM(bs)

	clientTLSConfig = &tls.Config{
		ServerName: "localhost",
		RootCAs:    cp,
	}
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randString(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func benchmarkEcho(b *testing.B, size int, accept func(net.Listener, *tls.Config) error, setup func(net.Addr), teardown func(), setupParallel func() func(string) string) {
	cert, err := tls.LoadX509KeyPair("server.crt", "server.key")
	if err != nil {
		b.Fatal(err)
	}

	listener, err := net.Listen("tcp4", ":0")
	if err != nil {
		b.Fatal(err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		NextProtos:   []string{"h2"},
	}

	listener = tls.NewListener(listener, tlsConfig)

	defer func() {
		if err := listener.Close(); err != nil {
			b.Fatal(err)
		}
	}()

	errChan := make(chan error)
	go func() {
		errChan <- accept(listener, tlsConfig)
		close(errChan)
	}()

	if setup != nil {
		setup(listener.Addr())
	}

	echoMsg := randString(size)

	b.SetBytes(2 * int64(len(echoMsg)))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {

		runRequest := setupParallel()

		for pb.Next() {
			if a, e := runRequest(echoMsg), echoMsg; a != e {
				b.Fatalf("expected:\n%q\ngot:\n%q", e, a)
			}
		}
	})

	b.StopTimer()

	if teardown != nil {
		teardown()
	}

	for {
		select {
		case err, ok := <-errChan:
			if !ok {
				return
			}
			if err != nil && !strings.HasSuffix(err.Error(), "use of closed network connection") {
				b.Fatal(err)
			}
		default:
			return
		}
	}
}

// grpc

type echoServer struct{}

func (e *echoServer) Echo(ctx context.Context, req *EchoRequest) (*EchoResponse, error) {
	return &EchoResponse{Msg: req.Msg}, nil
}

func (e *echoServer) StreamEcho(s Echo_StreamEchoServer) error {
	for {
		if s.Context().Err() != nil {
			return nil
		}
		req, err := s.Recv()
		if err != nil {
			return err
		}
		if err := s.Send(&EchoResponse{Msg: req.Msg}); err != nil {
			return err
		}
	}
}

func benchmarkEchoGRPC(b *testing.B, listenAndServeFn func(net.Listener, *tls.Config) error, size int) {
	var conn *grpc.ClientConn
	var client EchoClient
	benchmarkEcho(b, size, listenAndServeFn,
		func(addr net.Addr) {
			var err error

			_, port, err := net.SplitHostPort(addr.String())
			if err != nil {
				b.Fatal(err)
			}
			conn, err = grpc.Dial("localhost:"+port, grpc.WithTransportCredentials(credentials.NewTLS(clientTLSConfig)), grpc.WithBlock())
			if err != nil {
				b.Fatal(err)
			}
			client = NewEchoClient(conn)
		},
		func() {
			if err := conn.Close(); err != nil {
				b.Fatal(err)
			}
		},
		func() func(string) string {
			return func(echoMsg string) string {
				resp, err := client.Echo(context.Background(), &EchoRequest{Msg: echoMsg})
				if err != nil {
					b.Fatal(err)
				}
				return resp.Msg
			}
		},
	)
}

func benchmarkEchoStreamGRPC(b *testing.B, listenAndServeFn func(net.Listener, *tls.Config) error, size int) {
	var conn *grpc.ClientConn
	var client EchoClient
	benchmarkEcho(b, size, listenAndServeFn,
		func(addr net.Addr) {
			var err error
			_, port, err := net.SplitHostPort(addr.String())
			conn, err = grpc.Dial("localhost:"+port, grpc.WithTransportCredentials(credentials.NewTLS(clientTLSConfig)), grpc.WithBlock())
			if err != nil {
				b.Fatal(err)
			}
			client = NewEchoClient(conn)
		},
		func() {
			if err := conn.Close(); err != nil {
				b.Fatal(err)
			}
		},
		func() func(string) string {
			stream, err := client.StreamEcho(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			return func(echoMsg string) string {
				if err := stream.Send(&EchoRequest{Msg: echoMsg}); err != nil {
					b.Fatal(err)
				}
				resp, err := stream.Recv()
				if err != nil {
					b.Fatal(err)
				}
				return resp.Msg
			}
		},
	)
}

// Serve

func listenAndServeGRPCServe(listener net.Listener, _ *tls.Config) error {
	grpcServer := grpc.NewServer()
	RegisterEchoServer(grpcServer, new(echoServer))
	return grpcServer.Serve(listener)
}

func BenchmarkGRPCServe_1K(b *testing.B) {
	benchmarkEchoGRPC(b, listenAndServeGRPCServe, 1<<10)
}

func BenchmarkGRPCServe_64K(b *testing.B) {
	benchmarkEchoGRPC(b, listenAndServeGRPCServe, 64<<10)
}

func BenchmarkGRPCServe_Stream_1K(b *testing.B) {
	benchmarkEchoStreamGRPC(b, listenAndServeGRPCServe, 1<<10)
}

func BenchmarkGRPCServe_Stream_64k(b *testing.B) {
	benchmarkEchoStreamGRPC(b, listenAndServeGRPCServe, 64<<10)
}

// ServeHTTP

func listenAndServeGRPCServeHTTP(listener net.Listener, tlsConfig *tls.Config) error {
	grpcServer := grpc.NewServer()
	RegisterEchoServer(grpcServer, new(echoServer))
	srv := http.Server{
		TLSConfig: tlsConfig,
		Handler:   grpcServer,
	}

	return srv.Serve(listener)
}

func BenchmarkGRPCServeHTTP_1K(b *testing.B) {
	benchmarkEchoGRPC(b, listenAndServeGRPCServeHTTP, 1<<10)
}

func BenchmarkGRPCServeHTTP_64K(b *testing.B) {
	benchmarkEchoGRPC(b, listenAndServeGRPCServeHTTP, 64<<10)
}

func BenchmarkGRPCServeHTTP_Stream_1K(b *testing.B) {
	benchmarkEchoStreamGRPC(b, listenAndServeGRPCServeHTTP, 1<<10)
}

func BenchmarkGRPCServeHTTP_Stream_64k(b *testing.B) {
	benchmarkEchoStreamGRPC(b, listenAndServeGRPCServeHTTP, 64<<10)
}

// gob-rpc

type Echo struct{}

func (t *Echo) Echo(args *EchoRequest, reply *EchoResponse) error {
	reply.Msg = args.Msg
	return nil
}

func listenAndServeGobRPC(listener net.Listener, _ *tls.Config) error {
	rpcServer := rpc.NewServer()
	if err := rpcServer.Register(new(Echo)); err != nil {
		return err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go rpcServer.ServeConn(conn)
	}
}

func benchmarkEchoGobRPC(b *testing.B, size int) {
	var client *rpc.Client
	benchmarkEcho(b, size, listenAndServeGobRPC,
		func(addr net.Addr) {
			var err error
			conn, err := tls.Dial(addr.Network(), addr.String(), clientTLSConfig)
			if err != nil {
				b.Fatal(err)
			}
			client = rpc.NewClient(conn)
			if err != nil {
				b.Fatal(err)
			}
		},
		func() {
			if err := client.Close(); err != nil {
				b.Fatal(err)
			}
		},
		func() func(string) string {
			return func(echoMsg string) string {
				args := EchoRequest{Msg: echoMsg}
				var reply EchoResponse
				if err := client.Call("Echo.Echo", &args, &reply); err != nil {
					b.Fatal(err)
				}
				return reply.Msg
			}
		},
	)
}

func BenchmarkGobRPC_1K(b *testing.B) {
	benchmarkEchoGobRPC(b, 1<<10)
}

func BenchmarkGobRPC_64K(b *testing.B) {
	benchmarkEchoGobRPC(b, 64<<10)
}

// proto-http

const (
	contentType = "Content-Type"
	xProtobuf   = "application/x-protobuf"
)

func listenAndServeProtoHTTP(listener net.Listener, tlsConfig *tls.Config) error {
	srv := http.Server{
		TLSConfig: tlsConfig,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqBody, err := ioutil.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := r.Body.Close(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var args EchoRequest
			if err := proto.Unmarshal(reqBody, &args); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			reply := EchoResponse{Msg: args.Msg}
			respBody, err := proto.Marshal(&reply)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set(contentType, xProtobuf)
			w.Write(respBody)
		}),
	}

	return srv.Serve(listener)
}

func benchmarkEchoProtoHTTP(b *testing.B, size int, accept func(net.Listener, *tls.Config) error, roundTripper http.RoundTripper) {
	var url string
	benchmarkEcho(b, size, accept,
		func(addr net.Addr) {
			url = fmt.Sprintf("https://%s", addr)
		},
		nil,
		func() func(string) string {
			return func(echoMsg string) string {
				args := EchoRequest{Msg: echoMsg}
				reqBody, err := proto.Marshal(&args)
				if err != nil {
					b.Fatal(err)
				}
				req, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
				if err != nil {
					b.Fatal(err)
				}
				req.Header.Set("Content-Type", xProtobuf)
				resp, err := roundTripper.RoundTrip(req)
				if err != nil {
					b.Fatal(err)
				}
				respBody, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					b.Fatal(err)
				}
				if err := resp.Body.Close(); err != nil {
					b.Fatal(err)
				}
				var reply EchoResponse
				if err := proto.Unmarshal(respBody, &reply); err != nil {
					b.Fatal(err)
				}
				return reply.Msg
			}
		},
	)
}

func benchmarkEchoProtoHTTP1(b *testing.B, size int) {
	benchmarkEchoProtoHTTP(b, size, listenAndServeProtoHTTP, &http.Transport{
		TLSClientConfig: clientTLSConfig,
	})
}

func BenchmarkProtoHTTP1_1K(b *testing.B) {
	benchmarkEchoProtoHTTP1(b, 1<<10)
}

func BenchmarkProtoHTTP1_64K(b *testing.B) {
	benchmarkEchoProtoHTTP1(b, 64<<10)
}

func benchmarkEchoProtoHTTP2(b *testing.B, size int) {
	benchmarkEchoProtoHTTP(b, size, listenAndServeProtoHTTP, &http.Transport{
		TLSClientConfig: clientTLSConfig,
	})
}

func BenchmarkProtoHTTP2_1K(b *testing.B) {
	benchmarkEchoProtoHTTP2(b, 1<<10)
}

func BenchmarkProtoHTTP2_64K(b *testing.B) {
	benchmarkEchoProtoHTTP2(b, 64<<10)
}
