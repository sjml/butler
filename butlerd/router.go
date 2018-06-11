package butlerd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/itchio/wharf/werrors"

	"github.com/itchio/httpkit/neterr"
	"github.com/itchio/httpkit/progress"

	"crawshaw.io/sqlite"
	"github.com/itchio/butler/database/models"
	itchio "github.com/itchio/go-itchio"
	"github.com/itchio/wharf/state"
	"github.com/pkg/errors"
	"github.com/sourcegraph/jsonrpc2"
)

type RequestHandler func(rc *RequestContext) (interface{}, error)
type NotificationHandler func(rc *RequestContext)

type GetClientFunc func(key string) *itchio.Client

type Router struct {
	Handlers             map[string]RequestHandler
	NotificationHandlers map[string]NotificationHandler
	CancelFuncs          *CancelFuncs
	dbPool               *sqlite.Pool
	getClient            GetClientFunc

	ButlerVersion       string
	ButlerVersionString string
}

func NewRouter(dbPool *sqlite.Pool, getClient GetClientFunc) *Router {
	return &Router{
		Handlers:             make(map[string]RequestHandler),
		NotificationHandlers: make(map[string]NotificationHandler),
		CancelFuncs: &CancelFuncs{
			Funcs: make(map[string]context.CancelFunc),
		},
		dbPool:    dbPool,
		getClient: getClient,
	}
}

func (r *Router) Register(method string, rh RequestHandler) {
	if _, ok := r.Handlers[method]; ok {
		panic(fmt.Sprintf("Can't register handler twice for %s", method))
	}
	r.Handlers[method] = rh
}

func (r *Router) RegisterNotification(method string, nh NotificationHandler) {
	if _, ok := r.NotificationHandlers[method]; ok {
		panic(fmt.Sprintf("Can't register handler twice for %s", method))
	}
	r.NotificationHandlers[method] = nh
}

func (r *Router) Dispatch(ctx context.Context, origConn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	method := req.Method
	var res interface{}

	conn := &JsonRPC2Conn{origConn}
	consumer, cErr := NewStateConsumer(&NewStateConsumerParams{
		Ctx:  ctx,
		Conn: conn,
	})
	if cErr != nil {
		return
	}

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if rErr, ok := r.(error); ok {
					err = errors.WithStack(rErr)
				} else {
					err = errors.Errorf("panic: %v", r)
				}
			}
		}()

		rc := &RequestContext{
			Ctx:         ctx,
			Consumer:    consumer,
			Params:      req.Params,
			Conn:        conn,
			CancelFuncs: r.CancelFuncs,
			DBPool:      r.dbPool,
			Client:      r.getClient,

			ButlerVersion:       r.ButlerVersion,
			ButlerVersionString: r.ButlerVersionString,
		}

		if req.Notif {
			if nh, ok := r.NotificationHandlers[req.Method]; ok {
				nh(rc)
			}
		} else {
			if h, ok := r.Handlers[method]; ok {
				rc.Consumer.OnProgress = func(alpha float64) {
					if rc.tracker == nil {
						// skip
						return
					}

					rc.tracker.SetProgress(alpha)
					notif := &ProgressNotification{
						Progress: alpha,
						ETA:      rc.tracker.ETA().Seconds(),
						BPS:      rc.tracker.BPS(),
					}
					// cannot use autogenerated wrappers to avoid import cycles
					rc.Notify("Progress", notif)
				}
				rc.Consumer.OnProgressLabel = func(label string) {
					// muffin
				}
				rc.Consumer.OnPauseProgress = func() {
					if rc.tracker != nil {
						rc.tracker.Pause()
					}
				}
				rc.Consumer.OnResumeProgress = func() {
					if rc.tracker != nil {
						rc.tracker.Resume()
					}
				}

				res, err = h(rc)
			} else {
				err = &RpcError{
					Code:    jsonrpc2.CodeMethodNotFound,
					Message: fmt.Sprintf("Method '%s' not found", req.Method),
				}
			}
		}
		return
	}()

	if req.Notif {
		return
	}

	if err == nil {
		err = origConn.Reply(ctx, req.ID, res)
		if err != nil {
			consumer.Errorf("Error while replying: %s", err.Error())
		}
		return
	}

	var code int64
	var message string
	var data map[string]interface{}

	if ee, ok := AsButlerdError(err); ok {
		code = ee.RpcErrorCode()
		message = ee.RpcErrorMessage()
		data = ee.RpcErrorData()
	} else {
		if neterr.IsNetworkError(err) {
			code = int64(CodeNetworkDisconnected)
			message = CodeNetworkDisconnected.Error()
		} else if errors.Cause(err) == werrors.ErrCancelled {
			code = int64(CodeOperationCancelled)
			message = CodeOperationCancelled.Error()
		} else {
			code = jsonrpc2.CodeInternalError
			message = err.Error()
		}
	}

	var rawData *json.RawMessage
	if data == nil {
		data = make(map[string]interface{})
	}
	data["stack"] = fmt.Sprintf("%+v", err)
	data["butlerVersion"] = r.ButlerVersionString

	marshalledData, marshalErr := json.Marshal(data)
	if marshalErr == nil {
		rawMessage := json.RawMessage(marshalledData)
		rawData = &rawMessage
	}

	origConn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
		Code:    code,
		Message: message,
		Data:    rawData,
	})
}

type RequestContext struct {
	Ctx         context.Context
	Consumer    *state.Consumer
	Params      *json.RawMessage
	Conn        Conn
	CancelFuncs *CancelFuncs
	DBPool      *sqlite.Pool
	Client      GetClientFunc

	ButlerVersion       string
	ButlerVersionString string

	notificationInterceptors map[string]NotificationInterceptor
	tracker                  *progress.Tracker
}

type WithParamsFunc func() (interface{}, error)

type NotificationInterceptor func(method string, params interface{}) error

func (rc *RequestContext) Call(method string, params interface{}, res interface{}) error {
	return rc.Conn.Call(rc.Ctx, method, params, res)
}

func (rc *RequestContext) InterceptNotification(method string, interceptor NotificationInterceptor) {
	if rc.notificationInterceptors == nil {
		rc.notificationInterceptors = make(map[string]NotificationInterceptor)
	}
	rc.notificationInterceptors[method] = interceptor
}

func (rc *RequestContext) StopInterceptingNotification(method string) {
	if rc.notificationInterceptors == nil {
		return
	}
	delete(rc.notificationInterceptors, method)
}

func (rc *RequestContext) Notify(method string, params interface{}) error {
	if rc.notificationInterceptors != nil {
		if ni, ok := rc.notificationInterceptors[method]; ok {
			return ni(method, params)
		}
	}
	return rc.Conn.Notify(rc.Ctx, method, params)
}

func (rc *RequestContext) RootClient() *itchio.Client {
	return rc.Client("<keyless>")
}

func (rc *RequestContext) ProfileClient(profileID int64) (*models.Profile, *itchio.Client) {
	if profileID == 0 {
		panic(errors.New("profileId must be non-zero"))
	}

	conn := rc.DBPool.Get(rc.Ctx.Done())
	defer rc.DBPool.Put(conn)

	profile := models.ProfileByID(conn, profileID)
	if profile == nil {
		panic(errors.Errorf("Could not find profile %d", profileID))
	}

	if profile.APIKey == "" {
		panic(errors.Errorf("Profile %d lacks API key", profileID))
	}

	return profile, rc.Client(profile.APIKey)
}

func (rc *RequestContext) StartProgress() {
	rc.StartProgressWithTotalBytes(0)
}

func (rc *RequestContext) StartProgressWithTotalBytes(totalBytes int64) {
	rc.StartProgressWithInitialAndTotal(0.0, totalBytes)
}

func (rc *RequestContext) StartProgressWithInitialAndTotal(initialProgress float64, totalBytes int64) {
	if rc.tracker != nil {
		rc.Consumer.Warnf("Asked to start progress but already tracking progress!")
		return
	}

	rc.tracker = progress.NewTracker()
	rc.tracker.SetSilent(true)
	rc.tracker.SetProgress(initialProgress)
	rc.tracker.SetTotalBytes(totalBytes)
	rc.tracker.Start()
}

func (rc *RequestContext) EndProgress() {
	if rc.tracker != nil {
		rc.tracker.Finish()
		rc.tracker = nil
	} else {
		rc.Consumer.Warnf("Asked to stop progress but wasn't tracking progress!")
	}
}

func (rc *RequestContext) WithConn(f func(conn *sqlite.Conn)) {
	conn := rc.DBPool.Get(rc.Ctx.Done())
	defer rc.DBPool.Put(conn)
	f(conn)
}

func (rc *RequestContext) WithConnBool(f func(conn *sqlite.Conn) bool) bool {
	conn := rc.DBPool.Get(rc.Ctx.Done())
	defer rc.DBPool.Put(conn)
	return f(conn)
}

type CancelFuncs struct {
	Funcs map[string]context.CancelFunc
}

func (cf *CancelFuncs) Add(id string, f context.CancelFunc) {
	cf.Funcs[id] = f
}

func (cf *CancelFuncs) Remove(id string) {
	delete(cf.Funcs, id)
}

func (cf *CancelFuncs) Call(id string) bool {
	if f, ok := cf.Funcs[id]; ok {
		f()
		delete(cf.Funcs, id)
		return true
	}

	return false
}
