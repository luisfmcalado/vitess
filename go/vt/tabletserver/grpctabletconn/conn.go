// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gorpctabletconn

import (
	"fmt"
	"strings"
	"sync"
	"time"

	mproto "github.com/youtube/vitess/go/mysql/proto"
	"github.com/youtube/vitess/go/netutil"
	"github.com/youtube/vitess/go/rpcplus"
	"github.com/youtube/vitess/go/vt/rpc"
	tproto "github.com/youtube/vitess/go/vt/tabletserver/proto"
	"github.com/youtube/vitess/go/vt/tabletserver/tabletconn"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/vterrors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	pb "github.com/youtube/vitess/go/vt/proto/query"
	pbs "github.com/youtube/vitess/go/vt/proto/queryservice"
)

func init() {
	tabletconn.RegisterDialer("grpc", DialTablet)
}

// gRPCQueryClient implements a gRPC implementation for TabletConn
type gRPCQueryClient struct {
	mu        sync.RWMutex
	endPoint  topo.EndPoint
	cc        *grpc.ClientConn
	c         pbs.QueryClient
	sessionID int64
}

// DialTablet creates and initializes gRPCQueryClient.
func DialTablet(ctx context.Context, endPoint topo.EndPoint, keyspace, shard string, timeout time.Duration) (tabletconn.TabletConn, error) {
	// create the RPC client
	addr := netutil.JoinHostPort(endPoint.Host, endPoint.NamedPortMap["grpc"])
	cc, err := grpc.Dial(addr)
	if err != nil {
		return nil, err
	}
	c := pbs.NewQueryClient(cc)

	gsir, err := c.GetSessionId(ctx, &pb.GetSessionIdRequest{
		Keyspace: keyspace,
		Shard:    shard,
	})
	if err != nil {
		return nil, err
	}

	return &gRPCQueryClient{
		endPoint:  endPoint,
		cc:        cc,
		c:         c,
		sessionID: gsir.SessionId,
	}, nil
}

// Execute sends the query to VTTablet.
func (conn *gRPCQueryClient) Execute(ctx context.Context, query string, bindVars map[string]interface{}, transactionID int64) (*mproto.QueryResult, error) {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	if conn.cc == nil {
		return nil, tabletconn.ConnClosed
	}

	req := &pb.ExecuteRequest{
		Query:         tproto.BoundQueryToProto3(query, bindVars),
		TransactionId: transactionID,
		SessionId:     conn.sessionID,
	}
	er, err := conn.c.Execute(ctx, req)
	if err != nil {
		return nil, tabletError(err)
	}
	if er.Error != nil {
		return nil, vterrors.FromVtRPCError(er.Error)
	}
	return tproto.ProtoToQueryResult(er.Result), nil
}

// ExecuteBatch sends a batch query to VTTablet.
func (conn *gRPCQueryClient) ExecuteBatch(ctx context.Context, queries []tproto.BoundQuery, transactionID int64) (*tproto.QueryResultList, error) {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	if conn.cc == nil {
		return nil, tabletconn.ConnClosed
	}

	req := &pb.ExecuteBatchRequest{
		Queries:       make([]*pb.BoundQuery, len(queries)),
		TransactionId: transactionID,
		SessionId:     conn.sessionID,
	}
	for i, q := range queries {
		req.Queries[i] = tproto.BoundQueryToProto3(q.Sql, q.BindVariables)
	}
	ebr, err := conn.c.ExecuteBatch(ctx, req)
	if err != nil {
		return nil, tabletError(err)
	}
	if ebr.Error != nil {
		return nil, vterrors.FromVtRPCError(ebr.Error)
	}
	result := &tproto.QueryResultList{
		List: make([]mproto.QueryResult, len(ebr.Results)),
	}
	for i, qr := range ebr.Results {
		result.List[i] = *tproto.ProtoToQueryResult(qr)
	}
	return result, nil
}

// StreamExecute starts a streaming query to VTTablet.
func (conn *gRPCQueryClient) StreamExecute(ctx context.Context, query string, bindVars map[string]interface{}, transactionID int64) (<-chan *mproto.QueryResult, tabletconn.ErrFunc, error) {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	if conn.cc == nil {
		return nil, nil, tabletconn.ConnClosed
	}

	req := &tproto.Query{
		Sql:           query,
		BindVariables: bindVars,
		TransactionId: transactionID,
		SessionId:     conn.sessionID,
	}
	sr := make(chan *mproto.QueryResult, 10)
	c := conn.rpcClient.StreamGo("SqlQuery.StreamExecute", req, sr)
	firstResult, ok := <-sr
	if !ok {
		return nil, nil, tabletError(c.Error)
	}
	srout := make(chan *mproto.QueryResult, 1)
	go func() {
		defer close(srout)
		srout <- firstResult
		for r := range sr {
			srout <- r
		}
	}()
	return srout, func() error { return tabletError(c.Error) }, nil
}

// Begin starts a transaction.
func (conn *gRPCQueryClient) Begin(ctx context.Context) (transactionID int64, err error) {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	if conn.cc == nil {
		return 0, tabletconn.ConnClosed
	}

	req := &tproto.Session{
		SessionId: conn.sessionID,
	}
	var txInfo tproto.TransactionInfo
	action := func() error {
		return conn.rpcClient.Call(ctx, "SqlQuery.Begin", req, &txInfo)
	}
	err = conn.withTimeout(ctx, action)
	return txInfo.TransactionId, tabletError(err)
}

// Commit commits the ongoing transaction.
func (conn *gRPCQueryClient) Commit(ctx context.Context, transactionID int64) error {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	if conn.cc == nil {
		return tabletconn.ConnClosed
	}

	req := &tproto.Session{
		SessionId:     conn.sessionID,
		TransactionId: transactionID,
	}
	action := func() error {
		return conn.rpcClient.Call(ctx, "SqlQuery.Commit", req, &rpc.Unused{})
	}
	err := conn.withTimeout(ctx, action)
	return tabletError(err)
}

// Rollback rolls back the ongoing transaction.
func (conn *gRPCQueryClient) Rollback(ctx context.Context, transactionID int64) error {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	if conn.cc == nil {
		return tabletconn.ConnClosed
	}

	req := &tproto.Session{
		SessionId:     conn.sessionID,
		TransactionId: transactionID,
	}
	action := func() error {
		return conn.rpcClient.Call(ctx, "SqlQuery.Rollback", req, &rpc.Unused{})
	}
	err := conn.withTimeout(ctx, action)
	return tabletError(err)
}

// SplitQuery is the stub for SqlQuery.SplitQuery RPC
func (conn *gRPCQueryClient) SplitQuery(ctx context.Context, query tproto.BoundQuery, splitCount int) (queries []tproto.QuerySplit, err error) {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	if conn.cc == nil {
		err = tabletconn.ConnClosed
		return
	}
	req := &tproto.SplitQueryRequest{
		Query:      query,
		SplitCount: splitCount,
		SessionID:  conn.sessionID,
	}
	reply := new(tproto.SplitQueryResult)
	action := func() error {
		return conn.rpcClient.Call(ctx, "SqlQuery.SplitQuery", req, reply)
	}
	if err := conn.withTimeout(ctx, action); err != nil {
		return nil, tabletError(err)
	}
	return reply.Queries, nil
}

// Close closes underlying bsonrpc.
func (conn *gRPCQueryClient) Close() {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.cc == nil {
		return
	}

	conn.sessionID = 0
	cc := conn.cc
	conn.cc = nil
	cc.Close()
}

// EndPoint returns the rpc end point.
func (conn *gRPCQueryClient) EndPoint() topo.EndPoint {
	return conn.endPoint
}

func tabletError(err error) error {
	if err == nil {
		return nil
	}
	// TODO(aaijazi): tabletconn is in an intermediate state right now, where application errors
	// can be returned as rpcplus.ServerError or vterrors.VitessError. Soon, it will be standardized
	// to only VitessError.
	isServerError := false
	switch err.(type) {
	case rpcplus.ServerError:
		isServerError = true
	case *vterrors.VitessError:
		isServerError = true
	default:
	}
	if isServerError {
		var code int
		errStr := err.Error()
		switch {
		case strings.Contains(errStr, "fatal: "):
			code = tabletconn.ERR_FATAL
		case strings.Contains(errStr, "retry: "):
			code = tabletconn.ERR_RETRY
		case strings.Contains(errStr, "tx_pool_full: "):
			code = tabletconn.ERR_TX_POOL_FULL
		case strings.Contains(errStr, "not_in_tx: "):
			code = tabletconn.ERR_NOT_IN_TX
		default:
			code = tabletconn.ERR_NORMAL
		}
		return &tabletconn.ServerError{Code: code, Err: fmt.Sprintf("vttablet: %v", err)}
	}
	if err == context.Canceled {
		return tabletconn.Cancelled
	}
	return tabletconn.OperationalError(fmt.Sprintf("vttablet: %v", err))
}
