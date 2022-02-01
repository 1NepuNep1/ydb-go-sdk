package table

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_TableStats"

	"github.com/ydb-platform/ydb-go-sdk/v3/internal/cluster"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/errors"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/feature"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/operation"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/table/scanner"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/value"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/result"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/types"
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"
)

// session represents a single table API session.
//
// session methods are not goroutine safe. Simultaneous execution of requests
// are forbidden within a single session.
//
// Note that after session is no longer needed it should be destroyed by
// Close() call.
type session struct {
	id           string
	tableService Ydb_Table_V1.TableServiceClient
	trace        trace.Table

	closedMtx sync.RWMutex
	closed    bool

	statusMtx sync.RWMutex
	status    options.SessionStatus

	onCloseMtx sync.RWMutex
	onClose    []func(ctx context.Context)
}

func (s *session) NodeID() uint32 {
	u, err := url.Parse(s.id)
	if err != nil {
		panic(err)
	}
	nodeID, err := strconv.ParseUint(u.Query().Get("node_id"), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(nodeID)
}

func (s *session) Status() string {
	s.statusMtx.RLock()
	defer s.statusMtx.RUnlock()
	return s.status.String()
}

func (s *session) SetStatus(status options.SessionStatus) {
	s.statusMtx.Lock()
	s.status = status
	s.statusMtx.Unlock()
}

func (s *session) isClosed() bool {
	s.closedMtx.RLock()
	defer s.closedMtx.RUnlock()
	return s.closed
}

func (s *session) isClosing() bool {
	s.statusMtx.RLock()
	defer s.statusMtx.RUnlock()
	return s.status == options.SessionClosing
}

func newSession(ctx context.Context, cc grpc.ClientConnInterface, t trace.Table) (s Session, err error) {
	onDone := trace.TableOnSessionNew(t, &ctx)
	defer func() {
		onDone(s, err)
	}()
	var (
		response *Ydb_Table.CreateSessionResponse
		result   Ydb_Table.CreateSessionResult
	)
	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	c := Ydb_Table_V1.NewTableServiceClient(cc)
	response, err = c.CreateSession(
		ctx,
		&Ydb_Table.CreateSessionRequest{},
	)
	if err != nil {
		fmt.Println("exit 1")
		return nil, err
	}
	err = proto.Unmarshal(
		response.GetOperation().GetResult().GetValue(),
		&result,
	)
	if err != nil {
		fmt.Println("exit 2")
		return nil, err
	}
	fmt.Println("exit 3")
	return &session{
		id:           result.GetSessionId(),
		tableService: c,
		trace:        t,
	}, nil
}

func (s *session) trailer() *trailer {
	return &trailer{
		s:  s,
		md: metadata.MD{},
	}
}

func (s *session) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

func (s *session) OnClose(cb func(ctx context.Context)) {
	if s.isClosed() {
		return
	}
	s.onCloseMtx.Lock()
	s.onClose = append(s.onClose, cb)
	s.onCloseMtx.Unlock()
}

func (s *session) Close(ctx context.Context) (err error) {
	s.closedMtx.Lock()
	if s.closed {
		s.closedMtx.Unlock()
		return nil
	}
	s.closed = true
	s.closedMtx.Unlock()

	onDone := trace.TableOnSessionDelete(
		s.trace,
		&ctx,
		s,
	)
	defer func() {
		onDone(err)
	}()

	// call all close listeners before doing request
	// firstly this need to clear client from this session
	s.onCloseMtx.RLock()
	for _, cb := range s.onClose {
		cb(ctx)
	}
	s.onCloseMtx.RUnlock()

	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	t := s.trailer()
	defer t.Check()
	_, err = s.tableService.DeleteSession(
		cluster.WithEndpoint(ctx, s),
		&Ydb_Table.DeleteSessionRequest{
			SessionId: s.id,
		},
		t.Trailer(),
	)
	return err
}

// KeepAlive keeps idle session alive.
func (s *session) KeepAlive(ctx context.Context) (err error) {
	onDone := trace.TableOnSessionKeepAlive(
		s.trace,
		&ctx,
		s,
	)
	defer func() {
		onDone(err)
	}()
	var result Ydb_Table.KeepAliveResult
	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	t := s.trailer()
	defer t.Check()
	resp, err := s.tableService.KeepAlive(
		cluster.WithEndpoint(ctx, s),
		&Ydb_Table.KeepAliveRequest{
			SessionId: s.id,
		},
		t.Trailer(),
	)
	if err != nil {
		return
	}
	err = proto.Unmarshal(
		resp.GetOperation().GetResult().GetValue(),
		&result,
	)
	if err != nil {
		return
	}
	switch result.SessionStatus {
	case Ydb_Table.KeepAliveResult_SESSION_STATUS_READY:
		s.SetStatus(options.SessionReady)
	case Ydb_Table.KeepAliveResult_SESSION_STATUS_BUSY:
		s.SetStatus(options.SessionBusy)
	}
	return nil
}

// CreateTable creates table at given path with given options.
func (s *session) CreateTable(
	ctx context.Context,
	path string,
	opts ...options.CreateTableOption,
) (err error) {
	request := Ydb_Table.CreateTableRequest{
		SessionId: s.id,
		Path:      path,
	}
	for _, opt := range opts {
		opt((*options.CreateTableDesc)(&request))
	}
	t := s.trailer()
	defer t.Check()
	_, err = s.tableService.CreateTable(
		cluster.WithEndpoint(ctx, s),
		&request,
		t.Trailer(),
	)
	return err
}

// DescribeTable describes table at given path.
func (s *session) DescribeTable(
	ctx context.Context,
	path string,
	opts ...options.DescribeTableOption,
) (desc options.Description, err error) {
	var (
		response *Ydb_Table.DescribeTableResponse
		result   Ydb_Table.DescribeTableResult
	)
	request := Ydb_Table.DescribeTableRequest{
		SessionId: s.id,
		Path:      path,
	}
	for _, opt := range opts {
		opt((*options.DescribeTableDesc)(&request))
	}
	response, err = s.tableService.DescribeTable(
		cluster.WithEndpoint(ctx, s),
		&request,
	)
	if err != nil {
		return desc, err
	}
	err = proto.Unmarshal(
		response.GetOperation().GetResult().GetValue(),
		&result,
	)
	if err != nil {
		return
	}

	cs := make(
		[]options.Column,
		len(result.GetColumns()),
	)
	for i, c := range result.Columns {
		cs[i] = options.Column{
			Name:   c.GetName(),
			Type:   value.TypeFromYDB(c.GetType()),
			Family: c.GetFamily(),
		}
	}

	rs := make(
		[]options.KeyRange,
		len(result.GetShardKeyBounds())+1,
	)
	var last types.Value
	for i, b := range result.GetShardKeyBounds() {
		if last != nil {
			rs[i].From = last
		}

		bound := value.FromYDB(b.GetType(), b.GetValue())
		rs[i].To = bound

		last = bound
	}
	if last != nil {
		i := len(rs) - 1
		rs[i].From = last
	}

	var stats *options.TableStats
	if result.GetTableStats() != nil {
		resStats := result.GetTableStats()
		partStats := make(
			[]options.PartitionStats,
			len(result.GetTableStats().GetPartitionStats()),
		)
		for i, v := range result.TableStats.PartitionStats {
			partStats[i].RowsEstimate = v.GetRowsEstimate()
			partStats[i].StoreSize = v.GetStoreSize()
		}
		var creationTime, modificationTime time.Time
		if resStats.CreationTime.GetSeconds() != 0 {
			creationTime = time.Unix(
				resStats.GetCreationTime().GetSeconds(),
				int64(resStats.GetCreationTime().GetNanos()),
			)
		}
		if resStats.ModificationTime.GetSeconds() != 0 {
			modificationTime = time.Unix(
				resStats.GetModificationTime().GetSeconds(),
				int64(resStats.GetModificationTime().GetNanos()),
			)
		}

		stats = &options.TableStats{
			PartitionStats:   partStats,
			RowsEstimate:     resStats.GetRowsEstimate(),
			StoreSize:        resStats.GetStoreSize(),
			Partitions:       resStats.GetPartitions(),
			CreationTime:     creationTime,
			ModificationTime: modificationTime,
		}
	}

	cf := make([]options.ColumnFamily, len(result.GetColumnFamilies()))
	for i, c := range result.GetColumnFamilies() {
		cf[i] = options.NewColumnFamily(c)
	}

	attrs := make(map[string]string, len(result.GetAttributes()))
	for k, v := range result.GetAttributes() {
		attrs[k] = v
	}

	indexes := make([]options.IndexDescription, len(result.Indexes))
	for i, idx := range result.GetIndexes() {
		indexes[i] = options.IndexDescription{
			Name:         idx.GetName(),
			IndexColumns: idx.GetIndexColumns(),
			Status:       idx.GetStatus(),
		}
	}

	return options.Description{
		Name:                 result.GetSelf().GetName(),
		PrimaryKey:           result.GetPrimaryKey(),
		Columns:              cs,
		KeyRanges:            rs,
		Stats:                stats,
		ColumnFamilies:       cf,
		Attributes:           attrs,
		ReadReplicaSettings:  options.NewReadReplicasSettings(result.GetReadReplicasSettings()),
		StorageSettings:      options.NewStorageSettings(result.GetStorageSettings()),
		KeyBloomFilter:       feature.FromYDB(result.GetKeyBloomFilter()),
		PartitioningSettings: options.NewPartitioningSettings(result.GetPartitioningSettings()),
		Indexes:              indexes,
		TimeToLiveSettings:   options.NewTimeToLiveSettings(result.GetTtlSettings()),
	}, nil
}

// DropTable drops table at given path with given options.
func (s *session) DropTable(
	ctx context.Context,
	path string,
	opts ...options.DropTableOption,
) (err error) {
	request := Ydb_Table.DropTableRequest{
		SessionId: s.id,
		Path:      path,
	}
	for _, opt := range opts {
		opt((*options.DropTableDesc)(&request))
	}
	t := s.trailer()
	defer t.Check()
	_, err = s.tableService.DropTable(
		cluster.WithEndpoint(ctx, s),
		&request,
		t.Trailer(),
	)
	return err
}

// AlterTable modifies schema of table at given path with given options.
func (s *session) AlterTable(
	ctx context.Context,
	path string,
	opts ...options.AlterTableOption,
) (err error) {
	request := Ydb_Table.AlterTableRequest{
		SessionId: s.id,
		Path:      path,
	}
	for _, opt := range opts {
		opt((*options.AlterTableDesc)(&request))
	}
	t := s.trailer()
	defer t.Check()
	_, err = s.tableService.AlterTable(
		cluster.WithEndpoint(ctx, s),
		&request,
		t.Trailer(),
	)
	return err
}

// CopyTable creates copy of table at given path.
func (s *session) CopyTable(
	ctx context.Context,
	dst, src string,
	opts ...options.CopyTableOption,
) (err error) {
	request := Ydb_Table.CopyTableRequest{
		SessionId:       s.id,
		SourcePath:      src,
		DestinationPath: dst,
	}
	for _, opt := range opts {
		opt((*options.CopyTableDesc)(&request))
	}
	t := s.trailer()
	defer t.Check()
	_, err = s.tableService.CopyTable(
		cluster.WithEndpoint(ctx, s),
		&request,
		t.Trailer(),
	)
	return err
}

// Explain explains data query represented by text.
func (s *session) Explain(
	ctx context.Context,
	query string,
) (
	exp table.DataQueryExplanation,
	err error,
) {
	var (
		result   Ydb_Table.ExplainQueryResult
		response *Ydb_Table.ExplainDataQueryResponse
	)
	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	t := s.trailer()
	defer t.Check()
	response, err = s.tableService.ExplainDataQuery(
		cluster.WithEndpoint(ctx, s),
		&Ydb_Table.ExplainDataQueryRequest{
			SessionId: s.id,
			YqlText:   query,
		},
		t.Trailer(),
	)
	if err != nil {
		return
	}
	err = proto.Unmarshal(
		response.GetOperation().GetResult().GetValue(),
		&result,
	)
	if err != nil {
		return
	}
	return table.DataQueryExplanation{
		Explanation: table.Explanation{
			Plan: result.GetQueryPlan(),
		},
		AST: result.QueryAst,
	}, nil
}

// Statement is a prepared statement. Like a single session, it is not safe for
// concurrent use by multiple goroutines.
type Statement struct {
	session *session
	query   *dataQuery
	params  map[string]*Ydb.Type
}

// Execute executes prepared data query.
func (s *Statement) Execute(
	ctx context.Context, tx *table.TransactionControl,
	params *table.QueryParameters,
	opts ...options.ExecuteDataQueryOption,
) (
	txr table.Transaction, r result.Result, err error,
) {
	onDone := trace.TableOnSessionQueryExecute(
		s.session.trace,
		&ctx,
		s.session,
		s.query,
		params,
	)
	defer func() {
		onDone(txr, true, r, err)
	}()
	return s.execute(ctx, tx, params, opts...)
}

// execute executes prepared query without any tracing.
func (s *Statement) execute(
	ctx context.Context, tx *table.TransactionControl,
	params *table.QueryParameters,
	opts ...options.ExecuteDataQueryOption,
) (
	txr table.Transaction, r result.Result, err error,
) {
	_, res, err := s.session.executeDataQuery(
		ctx,
		tx,
		s.query,
		params,
		opts...,
	)
	if err != nil {
		return nil, nil, err
	}
	return s.session.executeQueryResult(res)
}

func (s *Statement) NumInput() int {
	return len(s.params)
}

func (s *Statement) Text() string {
	return s.query.YQL()
}

// Prepare prepares data query within session s.
func (s *session) Prepare(ctx context.Context, query string) (stmt table.Statement, err error) {
	var (
		q        *dataQuery
		response *Ydb_Table.PrepareDataQueryResponse
		result   Ydb_Table.PrepareQueryResult
	)
	onDone := trace.TableOnSessionQueryPrepare(
		s.trace,
		&ctx,
		s,
		query,
	)
	defer func() {
		onDone(q, err)
	}()

	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	t := s.trailer()
	defer t.Check()
	response, err = s.tableService.PrepareDataQuery(
		cluster.WithEndpoint(ctx, s),
		&Ydb_Table.PrepareDataQueryRequest{
			SessionId: s.id,
			YqlText:   query,
		},
		t.Trailer(),
	)
	if err != nil {
		return
	}
	err = proto.Unmarshal(
		response.GetOperation().GetResult().GetValue(),
		&result,
	)
	if err != nil {
		return
	}

	q = new(dataQuery)
	q.initPrepared(result.QueryId)
	stmt = &Statement{
		session: s,
		query:   q,
		params:  result.ParametersTypes,
	}

	return stmt, nil
}

// Execute executes given data query represented by text.
func (s *session) Execute(
	ctx context.Context,
	tx *table.TransactionControl,
	query string,
	params *table.QueryParameters,
	opts ...options.ExecuteDataQueryOption,
) (
	txr table.Transaction, r result.Result, err error,
) {
	q := new(dataQuery)
	q.initFromText(query)

	onDone := trace.TableOnSessionQueryExecute(s.trace, &ctx, s, q, params)
	defer func() {
		onDone(txr, true, r, err)
	}()

	request, result, err := s.executeDataQuery(ctx, tx, q, params, opts...)
	if err != nil {
		return nil, nil, err
	}
	if keepInCache(request) && result.QueryMeta != nil {
		queryID := result.QueryMeta.Id
		// Supplement q with ID for tracing.
		q.initPreparedText(query, queryID)
		// Create new dataQuery instead of q above to not store the whole query
		// string within statement.
		subq := new(dataQuery)
		subq.initPrepared(queryID)
	}

	return s.executeQueryResult(result)
}

func keepInCache(req *Ydb_Table.ExecuteDataQueryRequest) bool {
	p := req.QueryCachePolicy
	return p != nil && p.KeepInCache
}

// executeQueryResult returns Transaction and result built from received
// result.
func (s *session) executeQueryResult(res *Ydb_Table.ExecuteQueryResult) (
	table.Transaction,
	result.Result,
	error,
) {
	t := &Transaction{
		id: res.GetTxMeta().GetId(),
		s:  s,
	}
	r := scanner.NewUnary(
		res.GetResultSets(),
		res.GetQueryStats(),
	)
	return t, r, nil
}

// executeDataQuery executes data query.
func (s *session) executeDataQuery(
	ctx context.Context, tx *table.TransactionControl,
	query *dataQuery, params *table.QueryParameters,
	opts ...options.ExecuteDataQueryOption,
) (
	request *Ydb_Table.ExecuteDataQueryRequest,
	result *Ydb_Table.ExecuteQueryResult,
	err error,
) {
	var response *Ydb_Table.ExecuteDataQueryResponse
	result = &Ydb_Table.ExecuteQueryResult{}
	request = &Ydb_Table.ExecuteDataQueryRequest{
		SessionId:  s.id,
		TxControl:  tx.Desc(),
		Parameters: params.Params(),
		Query:      &query.query,
	}
	for _, opt := range opts {
		opt((*options.ExecuteDataQueryDesc)(request))
	}
	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	t := s.trailer()
	defer t.Check()
	response, err = s.tableService.ExecuteDataQuery(
		cluster.WithEndpoint(ctx, s),
		request,
		t.Trailer(),
	)
	if err != nil {
		return nil, nil, err
	}
	err = proto.Unmarshal(
		response.GetOperation().GetResult().GetValue(),
		result,
	)
	return request, result, err
}

// ExecuteSchemeQuery executes scheme query.
func (s *session) ExecuteSchemeQuery(
	ctx context.Context,
	query string,
	opts ...options.ExecuteSchemeQueryOption,
) (err error) {
	request := Ydb_Table.ExecuteSchemeQueryRequest{
		SessionId: s.id,
		YqlText:   query,
	}
	for _, opt := range opts {
		opt((*options.ExecuteSchemeQueryDesc)(&request))
	}
	t := s.trailer()
	defer t.Check()
	_, err = s.tableService.ExecuteSchemeQuery(
		cluster.WithEndpoint(ctx, s),
		&request,
		t.Trailer(),
	)
	return err
}

// DescribeTableOptions describes supported table options.
func (s *session) DescribeTableOptions(ctx context.Context) (
	desc options.TableOptionsDescription,
	err error,
) {
	var (
		response *Ydb_Table.DescribeTableOptionsResponse
		result   Ydb_Table.DescribeTableOptionsResult
	)
	request := Ydb_Table.DescribeTableOptionsRequest{}
	t := s.trailer()
	defer t.Check()
	response, err = s.tableService.DescribeTableOptions(
		cluster.WithEndpoint(ctx, s),
		&request,
		t.Trailer(),
	)
	if err != nil {
		return
	}
	err = proto.Unmarshal(
		response.GetOperation().GetResult().GetValue(),
		&result,
	)
	if err != nil {
		return
	}
	{
		xs := make([]options.TableProfileDescription, len(result.GetTableProfilePresets()))
		for i, p := range result.GetTableProfilePresets() {
			xs[i] = options.TableProfileDescription{
				Name:   p.GetName(),
				Labels: p.GetLabels(),

				DefaultStoragePolicy:      p.GetDefaultStoragePolicy(),
				DefaultCompactionPolicy:   p.GetDefaultCompactionPolicy(),
				DefaultPartitioningPolicy: p.GetDefaultPartitioningPolicy(),
				DefaultExecutionPolicy:    p.GetDefaultExecutionPolicy(),
				DefaultReplicationPolicy:  p.GetDefaultReplicationPolicy(),
				DefaultCachingPolicy:      p.GetDefaultCachingPolicy(),

				AllowedStoragePolicies:      p.GetAllowedStoragePolicies(),
				AllowedCompactionPolicies:   p.GetAllowedCompactionPolicies(),
				AllowedPartitioningPolicies: p.GetAllowedPartitioningPolicies(),
				AllowedExecutionPolicies:    p.GetAllowedExecutionPolicies(),
				AllowedReplicationPolicies:  p.GetAllowedReplicationPolicies(),
				AllowedCachingPolicies:      p.GetAllowedCachingPolicies(),
			}
		}
		desc.TableProfilePresets = xs
	}
	{
		xs := make(
			[]options.StoragePolicyDescription,
			len(result.GetStoragePolicyPresets()),
		)
		for i, p := range result.GetStoragePolicyPresets() {
			xs[i] = options.StoragePolicyDescription{
				Name:   p.GetName(),
				Labels: p.GetLabels(),
			}
		}
		desc.StoragePolicyPresets = xs
	}
	{
		xs := make(
			[]options.CompactionPolicyDescription,
			len(result.GetCompactionPolicyPresets()),
		)
		for i, p := range result.GetCompactionPolicyPresets() {
			xs[i] = options.CompactionPolicyDescription{
				Name:   p.GetName(),
				Labels: p.GetLabels(),
			}
		}
		desc.CompactionPolicyPresets = xs
	}
	{
		xs := make(
			[]options.PartitioningPolicyDescription,
			len(result.GetPartitioningPolicyPresets()),
		)
		for i, p := range result.GetPartitioningPolicyPresets() {
			xs[i] = options.PartitioningPolicyDescription{
				Name:   p.GetName(),
				Labels: p.GetLabels(),
			}
		}
		desc.PartitioningPolicyPresets = xs
	}
	{
		xs := make(
			[]options.ExecutionPolicyDescription,
			len(result.GetExecutionPolicyPresets()),
		)
		for i, p := range result.GetExecutionPolicyPresets() {
			xs[i] = options.ExecutionPolicyDescription{
				Name:   p.GetName(),
				Labels: p.GetLabels(),
			}
		}
		desc.ExecutionPolicyPresets = xs
	}
	{
		xs := make(
			[]options.ReplicationPolicyDescription,
			len(result.GetReplicationPolicyPresets()),
		)
		for i, p := range result.GetReplicationPolicyPresets() {
			xs[i] = options.ReplicationPolicyDescription{
				Name:   p.GetName(),
				Labels: p.GetLabels(),
			}
		}
		desc.ReplicationPolicyPresets = xs
	}
	{
		xs := make(
			[]options.CachingPolicyDescription,
			len(result.GetCachingPolicyPresets()),
		)
		for i, p := range result.GetCachingPolicyPresets() {
			xs[i] = options.CachingPolicyDescription{
				Name:   p.GetName(),
				Labels: p.GetLabels(),
			}
		}
		desc.CachingPolicyPresets = xs
	}
	return desc, nil
}

// StreamReadTable reads table at given path with given options.
//
// Note that given ctx controls the lifetime of the whole read, not only this
// StreamReadTable() call; that is, the time until returned result is closed
// via Close() call or fully drained by sequential NextResultSet() calls.
func (s *session) StreamReadTable(
	ctx context.Context,
	path string,
	opts ...options.ReadTableOption,
) (_ result.StreamResult, err error) {
	var (
		request = Ydb_Table.ReadTableRequest{
			SessionId: s.id,
			Path:      path,
		}
		stream Ydb_Table_V1.TableService_StreamReadTableClient
	)
	for _, opt := range opts {
		opt((*options.ReadTableDesc)(&request))
	}

	ctx, cancel := context.WithCancel(ctx)

	stream, err = s.tableService.StreamReadTable(
		cluster.WithEndpoint(ctx, s),
		&request,
	)

	onDone := trace.TableOnSessionQueryStreamRead(s.trace, &ctx, s)
	if err != nil {
		cancel()
		onDone(err)
		return nil, err
	}

	return scanner.NewStream(
		func(ctx context.Context) (
			set *Ydb.ResultSet,
			stats *Ydb_TableStats.QueryStats,
			err error,
		) {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			default:
				response, err := stream.Recv()
				result := response.GetResult()
				if result == nil || err != nil {
					return nil, nil, err
				}
				return result.GetResultSet(), nil, nil
			}
		},
		func(err error) error {
			cancel()
			onDone(err)
			if checkHintSessionClose(stream.Trailer()) {
				s.SetStatus(options.SessionClosing)
			}
			return err
		},
	), nil
}

// StreamExecuteScanQuery scan-reads table at given path with given options.
//
// Note that given ctx controls the lifetime of the whole read, not only this
// StreamExecuteScanQuery() call; that is, the time until returned result is closed
// via Close() call or fully drained by sequential NextResultSet() calls.
func (s *session) StreamExecuteScanQuery(
	ctx context.Context,
	query string,
	params *table.QueryParameters,
	opts ...options.ExecuteScanQueryOption,
) (_ result.StreamResult, err error) {
	q := new(dataQuery)
	q.initFromText(query)
	var (
		request = Ydb_Table.ExecuteScanQueryRequest{
			Query:      &q.query,
			Parameters: params.Params(),
			Mode:       Ydb_Table.ExecuteScanQueryRequest_MODE_EXEC, // set default
		}
		stream Ydb_Table_V1.TableService_StreamExecuteScanQueryClient
	)
	for _, opt := range opts {
		opt((*options.ExecuteScanQueryDesc)(&request))
	}

	ctx, cancel := context.WithCancel(ctx)

	stream, err = s.tableService.StreamExecuteScanQuery(
		cluster.WithEndpoint(ctx, s),
		&request,
	)

	onDone := trace.TableOnSessionQueryStreamExecute(
		s.trace,
		&ctx,
		s,
		q,
		params,
	)
	if err != nil {
		cancel()
		onDone(err)
		return nil, err
	}

	return scanner.NewStream(
		func(ctx context.Context) (
			set *Ydb.ResultSet,
			stats *Ydb_TableStats.QueryStats,
			err error,
		) {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			default:
				response, err := stream.Recv()
				result := response.GetResult()
				if result == nil || err != nil {
					return nil, nil, err
				}
				return result.GetResultSet(), result.GetQueryStats(), nil
			}
		},
		func(err error) error {
			cancel()
			onDone(err)
			if checkHintSessionClose(stream.Trailer()) {
				s.SetStatus(options.SessionClosing)
			}
			return err
		},
	), nil
}

// BulkUpsert uploads given list of ydb struct values to the table.
func (s *session) BulkUpsert(ctx context.Context, table string, rows types.Value) (err error) {
	t := s.trailer()
	defer t.Check()
	_, err = s.tableService.BulkUpsert(
		cluster.WithEndpoint(ctx, s),
		&Ydb_Table.BulkUpsertRequest{
			Table: table,
			Rows:  value.ToYDB(rows),
		},
		t.Trailer(),
	)
	return err
}

// BeginTransaction begins new transaction within given session with given
// settings.
func (s *session) BeginTransaction(
	ctx context.Context,
	tx *table.TransactionSettings,
) (x table.Transaction, err error) {
	onDone := trace.TableOnSessionTransactionBegin(
		s.trace,
		&ctx,
		s,
	)
	defer func() {
		onDone(x, err)
	}()
	var (
		result   Ydb_Table.BeginTransactionResult
		response *Ydb_Table.BeginTransactionResponse
	)
	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	t := s.trailer()
	defer t.Check()
	response, err = s.tableService.BeginTransaction(
		cluster.WithEndpoint(ctx, s),
		&Ydb_Table.BeginTransactionRequest{
			SessionId:  s.id,
			TxSettings: tx.Settings(),
		},
		t.Trailer(),
	)
	if err != nil {
		return nil, err
	}
	err = proto.Unmarshal(
		response.GetOperation().GetResult().GetValue(),
		&result,
	)
	if err != nil {
		return
	}
	return &Transaction{
		id: result.GetTxMeta().GetId(),
		s:  s,
	}, nil
}

// Transaction is a database transaction.
// Hence session methods are not goroutine safe, Transaction is not goroutine
// safe either.
type Transaction struct {
	id string
	s  *session
	c  *table.TransactionControl

	committed bool
}

func (tx *Transaction) ID() string {
	return tx.id
}

func (tx *Transaction) IsNil() bool {
	return tx == nil
}

// Execute executes query represented by text within transaction tx.
func (tx *Transaction) Execute(
	ctx context.Context,
	query string, params *table.QueryParameters,
	opts ...options.ExecuteDataQueryOption,
) (r result.Result, err error) {
	_, r, err = tx.s.Execute(ctx, tx.txc(), query, params, opts...)
	return
}

// ExecuteStatement executes prepared statement stmt within transaction tx.
func (tx *Transaction) ExecuteStatement(
	ctx context.Context,
	stmt table.Statement, params *table.QueryParameters,
	opts ...options.ExecuteDataQueryOption,
) (r result.Result, err error) {
	_, r, err = stmt.Execute(ctx, tx.txc(), params, opts...)
	return
}

// CommitTx commits specified active transaction.
func (tx *Transaction) CommitTx(
	ctx context.Context,
	opts ...options.CommitTransactionOption,
) (r result.Result, err error) {
	if tx.committed {
		return nil, errors.ErrAlreadyCommited
	}
	defer func() {
		if err == nil {
			tx.committed = true
		}
	}()
	onDone := trace.TableOnSessionTransactionCommit(
		tx.s.trace,
		&ctx,
		tx.s,
		tx,
	)
	defer func() {
		onDone(err)
	}()
	var (
		request = &Ydb_Table.CommitTransactionRequest{
			SessionId: tx.s.id,
			TxId:      tx.id,
		}
		response *Ydb_Table.CommitTransactionResponse
		result   = new(Ydb_Table.CommitTransactionResult)
	)
	for _, opt := range opts {
		opt((*options.CommitTransactionDesc)(request))
	}
	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	t := tx.s.trailer()
	defer t.Check()
	response, err = tx.s.tableService.CommitTransaction(
		cluster.WithEndpoint(ctx, tx.s),
		request,
		t.Trailer(),
	)
	if err != nil {
		return nil, err
	}
	err = proto.Unmarshal(
		response.GetOperation().GetResult().GetValue(),
		result,
	)
	if err != nil {
		return nil, err
	}
	return scanner.NewUnary(
		nil,
		result.GetQueryStats(),
	), nil
}

// Rollback performs a rollback of the specified active transaction.
func (tx *Transaction) Rollback(ctx context.Context) (err error) {
	if tx.committed {
		return nil
	}
	onDone := trace.TableOnSessionTransactionRollback(
		tx.s.trace,
		&ctx,
		tx.s,
		tx,
	)
	defer func() {
		onDone(err)
	}()
	if m, _ := operation.ContextMode(ctx); m == operation.ModeUnknown {
		ctx = operation.WithMode(ctx, operation.ModeSync)
	}
	t := tx.s.trailer()
	defer t.Check()
	_, err = tx.s.tableService.RollbackTransaction(
		cluster.WithEndpoint(ctx, tx.s),
		&Ydb_Table.RollbackTransactionRequest{
			SessionId: tx.s.id,
			TxId:      tx.id,
		},
		t.Trailer(),
	)
	return err
}

func (tx *Transaction) txc() *table.TransactionControl {
	if tx.c == nil {
		tx.c = table.TxControl(table.WithTx(tx))
	}
	return tx.c
}

type dataQuery struct {
	query    Ydb_Table.Query
	queryID  Ydb_Table.Query_Id
	queryYQL Ydb_Table.Query_YqlText
}

func (q *dataQuery) String() string {
	var emptyID Ydb_Table.Query_Id
	if q.queryID == emptyID {
		return q.queryYQL.YqlText
	}
	return q.queryID.Id
}

func (q *dataQuery) ID() string {
	return q.queryID.Id
}

func (q *dataQuery) YQL() string {
	return q.queryYQL.YqlText
}

func (q *dataQuery) initFromText(s string) {
	q.queryID = Ydb_Table.Query_Id{} // Reset id field.
	q.queryYQL.YqlText = s
	q.query.Query = &q.queryYQL
}

func (q *dataQuery) initPrepared(id string) {
	q.queryYQL = Ydb_Table.Query_YqlText{} // Reset yql field.
	q.queryID.Id = id
	q.query.Query = &q.queryID
}

func (q *dataQuery) initPreparedText(s, id string) {
	q.queryYQL = Ydb_Table.Query_YqlText{} // Reset yql field.
	q.queryYQL.YqlText = s

	q.queryID = Ydb_Table.Query_Id{} // Reset id field.
	q.queryID.Id = id

	q.query.Query = &q.queryID // Prefer preared query.
}
