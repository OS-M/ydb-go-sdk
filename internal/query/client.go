package query

import (
	"context"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Query_V1"
	"google.golang.org/grpc"

	"github.com/ydb-platform/ydb-go-sdk/v3/internal/pool"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/pool/stats"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/query/config"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/query/options"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/stack"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xcontext"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xerrors"
	"github.com/ydb-platform/ydb-go-sdk/v3/query"
	"github.com/ydb-platform/ydb-go-sdk/v3/retry"
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"
)

//go:generate mockgen -destination grpc_client_mock_test.go -package query -write_package_comment=false github.com/ydb-platform/ydb-go-genproto/Ydb_Query_V1 QueryServiceClient,QueryService_AttachSessionClient,QueryService_ExecuteQueryClient

type nodeChecker interface {
	HasNode(id uint32) bool
}

type balancer interface {
	grpc.ClientConnInterface
	nodeChecker
}

var _ query.Client = (*Client)(nil)

type Client struct {
	config     *config.Config
	grpcClient Ydb_Query_V1.QueryServiceClient
	pool       *pool.Pool[*Session, Session]

	done chan struct{}
}

func (c *Client) Stats() *stats.Stats {
	s := c.pool.Stats()

	return &s
}

func (c *Client) Close(ctx context.Context) error {
	close(c.done)

	if err := c.pool.Close(ctx); err != nil {
		return xerrors.WithStackTrace(err)
	}

	return nil
}

func do(
	ctx context.Context,
	pool *pool.Pool[*Session, Session],
	op query.Operation,
	opts ...retry.Option,
) (finalErr error) {
	err := pool.With(ctx, func(ctx context.Context, s *Session) error {
		s.setStatus(statusInUse)

		err := op(ctx, s)
		if err != nil {
			if !xerrors.IsRetryObjectValid(err) {
				s.setStatus(statusError)
			}

			return xerrors.WithStackTrace(err)
		}

		s.setStatus(statusIdle)

		return nil
	}, opts...)
	if err != nil {
		return xerrors.WithStackTrace(err)
	}

	return nil
}

func doWithAttempts(
	ctx context.Context,
	pool *pool.Pool[*Session, Session],
	op query.Operation,
	t *trace.Query,
	opts ...options.DoOption,
) (attempts int, finalErr error) {
	doOpts := options.ParseDoOpts(t, opts...)

	err := do(ctx, pool, op,
		append(doOpts.RetryOpts(), retry.WithTrace(&trace.Retry{
			OnRetry: func(info trace.RetryLoopStartInfo) func(trace.RetryLoopDoneInfo) {
				return func(info trace.RetryLoopDoneInfo) {
					attempts = info.Attempts
				}
			},
		}))...,
	)
	if err != nil {
		return attempts, xerrors.WithStackTrace(err)
	}

	return attempts, nil
}

func (c *Client) Do(ctx context.Context, op query.Operation, opts ...options.DoOption) error {
	ctx, cancel := xcontext.WithDone(ctx, c.done)
	defer cancel()

	onDone := trace.QueryOnDo(c.config.Trace(), &ctx,
		stack.FunctionID("github.com/ydb-platform/ydb-go-sdk/3/internal/query.(*Client).Do"),
	)
	attempts, err := doWithAttempts(ctx, c.pool, op, c.config.Trace(), opts...)
	onDone(attempts, err)

	return err
}

func doTxWithAttempts(
	ctx context.Context,
	pool *pool.Pool[*Session, Session],
	op query.TxOperation,
	t *trace.Query,
	opts ...options.DoTxOption,
) (attempts int, err error) {
	doTxOpts := options.ParseDoTxOpts(t, opts...)

	attempts, err = doWithAttempts(ctx, pool, func(ctx context.Context, s query.Session) (err error) {
		tx, err := s.Begin(ctx, doTxOpts.TxSettings())
		if err != nil {
			return xerrors.WithStackTrace(err)
		}
		err = op(ctx, tx)
		if err != nil {
			errRollback := tx.Rollback(ctx)
			if errRollback != nil {
				return xerrors.WithStackTrace(xerrors.Join(err, errRollback))
			}

			return xerrors.WithStackTrace(err)
		}
		err = tx.CommitTx(ctx)
		if err != nil {
			errRollback := tx.Rollback(ctx)
			if errRollback != nil {
				return xerrors.WithStackTrace(xerrors.Join(err, errRollback))
			}

			return xerrors.WithStackTrace(err)
		}

		return nil
	}, t, doTxOpts.DoOpts()...)
	if err != nil {
		return attempts, xerrors.WithStackTrace(err)
	}

	return attempts, nil
}

// ReadRow is a helper which read only one row from first result set in result
func (c *Client) ReadRow(ctx context.Context, q string, opts ...options.ExecuteOption) (row query.Row, err error) {
	ctx, cancel := xcontext.WithDone(ctx, c.done)
	defer cancel()

	onDone := trace.QueryOnReadRow(c.config.Trace(), &ctx,
		stack.FunctionID("github.com/ydb-platform/ydb-go-sdk/3/internal/query.(*Client).ReadRow"),
		q,
	)
	defer func() {
		onDone(err)
	}()

	err = do(ctx, c.pool, func(ctx context.Context, s query.Session) (err error) {
		row, err = s.ReadRow(ctx, q, opts...)
		if err != nil {
			return xerrors.WithStackTrace(err)
		}

		return nil
	})
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	return row, nil
}

func clientExecute(ctx context.Context,
	pool *pool.Pool[*Session, Session],
	q string, opts ...options.ExecuteOption,
) (r query.Result, err error) {
	err = do(ctx, pool, func(ctx context.Context, s query.Session) (err error) {
		_, r, err = s.Execute(ctx, q, opts...)
		if err != nil {
			return xerrors.WithStackTrace(err)
		}
		defer func() {
			_ = r.Close(ctx)
		}()

		r, err = resultToMaterializedResult(ctx, r)
		if err != nil {
			return xerrors.WithStackTrace(err)
		}

		if err = r.Err(); err != nil {
			return xerrors.WithStackTrace(err)
		}

		return nil
	})
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	return r, nil
}

func (c *Client) Execute(ctx context.Context, q string, opts ...options.ExecuteOption) (_ query.Result, err error) {
	ctx, cancel := xcontext.WithDone(ctx, c.done)
	defer cancel()

	onDone := trace.QueryOnExecute(c.config.Trace(), &ctx,
		stack.FunctionID("github.com/ydb-platform/ydb-go-sdk/3/internal/query.(*Client).Execute"),
		q,
	)
	defer func() {
		onDone(err)
	}()

	r, err := clientExecute(ctx, c.pool, q, opts...)
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	return r, nil
}

// ReadResultSet is a helper which read all rows from first result set in result
func (c *Client) ReadResultSet(
	ctx context.Context, q string, opts ...options.ExecuteOption,
) (rs query.ResultSet, err error) {
	ctx, cancel := xcontext.WithDone(ctx, c.done)
	defer cancel()

	onDone := trace.QueryOnReadResultSet(c.config.Trace(), &ctx,
		stack.FunctionID("github.com/ydb-platform/ydb-go-sdk/3/internal/query.(*Client).ReadResultSet"),
		q,
	)
	defer func() {
		onDone(err)
	}()

	err = do(ctx, c.pool, func(ctx context.Context, s query.Session) (err error) {
		rs, err = s.ReadResultSet(ctx, q, opts...)
		if err != nil {
			return xerrors.WithStackTrace(err)
		}

		return nil
	})
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	return rs, nil
}

func (c *Client) DoTx(ctx context.Context, op query.TxOperation, opts ...options.DoTxOption) (err error) {
	ctx, cancel := xcontext.WithDone(ctx, c.done)
	defer cancel()

	onDone := trace.QueryOnDoTx(c.config.Trace(), &ctx,
		stack.FunctionID("github.com/ydb-platform/ydb-go-sdk/3/internal/query.(*Client).DoTx"),
	)
	attempts, err := doTxWithAttempts(ctx, c.pool, op, c.config.Trace(), opts...)
	onDone(attempts, err)

	return err
}

func New(ctx context.Context, balancer balancer, cfg *config.Config) *Client {
	onDone := trace.QueryOnNew(cfg.Trace(), &ctx,
		stack.FunctionID("github.com/ydb-platform/ydb-go-sdk/3/internal/query.New"),
	)
	defer onDone()

	client := &Client{
		config:     cfg,
		grpcClient: Ydb_Query_V1.NewQueryServiceClient(balancer),
		done:       make(chan struct{}),
	}

	client.pool = pool.New(ctx,
		pool.WithLimit[*Session, Session](cfg.PoolLimit()),
		pool.WithTrace[*Session, Session](poolTrace(cfg.Trace())),
		pool.WithCreateItemTimeout[*Session, Session](cfg.SessionCreateTimeout()),
		pool.WithCloseItemTimeout[*Session, Session](cfg.SessionDeleteTimeout()),
		pool.WithCreateFunc(func(ctx context.Context) (_ *Session, err error) {
			var (
				createCtx    context.Context
				cancelCreate context.CancelFunc
			)
			if d := cfg.SessionCreateTimeout(); d > 0 {
				createCtx, cancelCreate = xcontext.WithTimeout(ctx, d)
			} else {
				createCtx, cancelCreate = xcontext.WithCancel(ctx)
			}
			defer cancelCreate()

			s, err := createSession(createCtx, client.grpcClient, cfg,
				withSessionCheck(func(s *Session) bool {
					return balancer.HasNode(uint32(s.nodeID))
				}),
			)
			if err != nil {
				return nil, xerrors.WithStackTrace(err)
			}

			return s, nil
		}),
	)

	return client
}

func poolTrace(t *trace.Query) *pool.Trace {
	return &pool.Trace{
		OnNew: func(info *pool.NewStartInfo) func(*pool.NewDoneInfo) {
			onDone := trace.QueryOnPoolNew(t, info.Context, info.Call)

			return func(info *pool.NewDoneInfo) {
				onDone(info.Limit)
			}
		},
		OnClose: func(info *pool.CloseStartInfo) func(*pool.CloseDoneInfo) {
			onDone := trace.QueryOnClose(t, info.Context, info.Call)

			return func(info *pool.CloseDoneInfo) {
				onDone(info.Error)
			}
		},
		OnTry: func(info *pool.TryStartInfo) func(*pool.TryDoneInfo) {
			onDone := trace.QueryOnPoolTry(t, info.Context, info.Call)

			return func(info *pool.TryDoneInfo) {
				onDone(info.Error)
			}
		},
		OnWith: func(info *pool.WithStartInfo) func(*pool.WithDoneInfo) {
			onDone := trace.QueryOnPoolWith(t, info.Context, info.Call)

			return func(info *pool.WithDoneInfo) {
				onDone(info.Error, info.Attempts)
			}
		},
		OnPut: func(info *pool.PutStartInfo) func(*pool.PutDoneInfo) {
			onDone := trace.QueryOnPoolPut(t, info.Context, info.Call)

			return func(info *pool.PutDoneInfo) {
				onDone(info.Error)
			}
		},
		OnGet: func(info *pool.GetStartInfo) func(*pool.GetDoneInfo) {
			onDone := trace.QueryOnPoolGet(t, info.Context, info.Call)

			return func(info *pool.GetDoneInfo) {
				onDone(info.Error)
			}
		},
		OnChange: func(info pool.ChangeInfo) {
			trace.QueryOnPoolChange(t, info.Limit, info.Index, info.Idle, info.InUse)
		},
	}
}
