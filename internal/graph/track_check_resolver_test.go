package graph

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"
	"github.com/stretchr/testify/require"

	"go.uber.org/goleak"
	"go.uber.org/mock/gomock"

	"github.com/openfga/openfga/pkg/logger"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/memory"
	"github.com/openfga/openfga/pkg/tuple"
	"github.com/openfga/openfga/pkg/typesystem"
)

func TestIntegrationTracker(t *testing.T) {
	t.Cleanup(func() {
		goleak.VerifyNone(t)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := logger.NewNoopLogger()
	cycleDetectionCheckResolver := NewCycleDetectionCheckResolver()
	t.Cleanup(cycleDetectionCheckResolver.Close)

	localChecker := NewLocalChecker()
	t.Cleanup(localChecker.Close)

	trackChecker := NewTrackCheckResolver(
		WithTrackerContext(ctx),
		WithTrackerLogger(logger))
	t.Cleanup(trackChecker.Close)

	cycleDetectionCheckResolver.SetDelegate(trackChecker)
	trackChecker.SetDelegate(localChecker)
	localChecker.SetDelegate(cycleDetectionCheckResolver)

	t.Run("tracker_integrates_with_cycle_and_local_checker", func(t *testing.T) {
		ds := memory.New()
		t.Cleanup(ds.Close)

		storeID := ulid.Make().String()

		model := parser.MustTransformDSLToProto(`
		model
		  schema 1.1

		type user

		type group
		  relations
			define blocked: [user, group#member]
			define member: [user, group#member] but not blocked
`)

		err := ds.Write(
			ctx,
			storeID,
			nil,
			[]*openfgav1.TupleKey{
				tuple.NewTupleKey("group:1", "member", "user:jon"),
				tuple.NewTupleKey("group:2", "blocked", "group:1#member"),
				tuple.NewTupleKey("group:3", "blocked", "group:1#member"),
			})
		require.NoError(t, err)

		typesys, err := typesystem.NewAndValidate(
			context.Background(),
			model,
		)
		require.NoError(t, err)

		ctx = storage.ContextWithRelationshipTupleReader(ctx, ds)
		ctx = typesystem.ContextWithTypesystem(ctx, typesys)
		resp, err := trackChecker.ResolveCheck(ctx, &ResolveCheckRequest{
			AuthorizationModelID: ulid.Make().String(),
			StoreID:              storeID,
			TupleKey:             tuple.NewTupleKey("group:1", "blocked", "user:jon"),
			RequestMetadata:      NewCheckRequestMetadata(25),
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.False(t, resp.GetAllowed())
	})

	t.Run("tracker_delegates_request", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLocalChecker := NewMockCheckResolver(ctrl)
		mockLocalChecker.EXPECT().ResolveCheck(gomock.Any(), gomock.Any()).Return(&ResolveCheckResponse{
			Allowed: true,
		}, nil).Times(1)
		trackChecker.SetDelegate(mockLocalChecker)

		resp, err := trackChecker.ResolveCheck(ctx, &ResolveCheckRequest{
			StoreID:         ulid.Make().String(),
			TupleKey:        tuple.NewTupleKey("document:1", "viewer", "user:will"),
			RequestMetadata: NewCheckRequestMetadata(defaultResolveNodeLimit),
			VisitedPaths:    map[string]struct{}{},
		})

		require.NoError(t, err)
		require.True(t, resp.GetAllowed())
	})

	t.Run("tracker_user_type_and_expiration", func(t *testing.T) {
		userType := trackChecker.userType("group:1#member")
		require.Equal(t, "userset", userType)

		userType = trackChecker.userType("user:ann")
		require.Equal(t, "user", userType)

		userType = trackChecker.userType("user:*")
		require.Equal(t, "userset", userType)

		r := resolutionNode{tm: time.Now().Add(-trackerInterval)}
		require.True(t, r.expired())

		r = resolutionNode{tm: time.Now().Add(trackerInterval)}
		require.False(t, r.expired())
	})

	t.Run("tracker_prints_and_delete_path", func(t *testing.T) {
		r := &ResolveCheckRequest{
			StoreID:              ulid.Make().String(),
			AuthorizationModelID: ulid.Make().String(),
			TupleKey:             tuple.NewTupleKey("document:abc", "viewer", "user:somebody"),
			RequestMetadata:      NewCheckRequestMetadata(20),
		}
		value, ok := trackChecker.loadModel(r)
		require.NotNil(t, value)
		require.False(t, ok)

		path := "group:1#member@user"
		sm := &sync.Map{}
		sm.Store(
			path,
			&resolutionNode{
				tm:   time.Now().Add(-trackerInterval),
				hits: &atomic.Uint64{},
			},
		)

		trackChecker.nodes.Store(trackerKey{store: ulid.Make().String(), model: ulid.Make().String()}, sm)
		trackChecker.logExecutionPaths(false)

		_, ok = sm.Load(path)
		require.False(t, ok)
	})

	t.Run("tracker_success_launch_flush", func(t *testing.T) {
		wg := sync.WaitGroup{}

		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(10) * time.Millisecond)
		}()

		path := "group:1#member@user"
		sm := &sync.Map{}
		sm.Store(
			path,
			&resolutionNode{
				tm:   time.Now().Add(-trackerInterval),
				hits: &atomic.Uint64{},
			},
		)

		trackChecker.nodes.Store(trackerKey{store: ulid.Make().String(), model: ulid.Make().String()}, sm)

		trackChecker.ticker.Reset(time.Duration(2) * time.Millisecond)
		trackChecker.launchFlush()

		wg.Wait()

		_, ok := sm.Load(path)
		require.False(t, ok)
	})

	t.Run("logExecutionPaths_limiter_disallow", func(t *testing.T) {
		r := &ResolveCheckRequest{
			StoreID:              ulid.Make().String(),
			AuthorizationModelID: ulid.Make().String(),
			TupleKey:             tuple.NewTupleKey("document:abc", "viewer", "user:somebody"),
			RequestMetadata:      NewCheckRequestMetadata(20),
		}
		value, ok := trackChecker.loadModel(r)
		require.NotNil(t, value)
		require.False(t, ok)

		path := trackChecker.getTK(r.GetTupleKey())
		trackChecker.loadPath(value, path)
		trackChecker.addPathHits(r)

		paths, ok := value.(*sync.Map)
		require.True(t, ok)
		_, ok = paths.Load(path)
		require.True(t, ok)

		paths, ok = value.(*sync.Map)
		require.True(t, ok)
		_, ok = paths.Load(path)
		require.True(t, ok)

		oldBurst := trackChecker.limiter.Burst()
		trackChecker.limiter.SetBurst(0)

		trackChecker.logExecutionPaths(true)
		trackChecker.limiter.SetBurst(oldBurst)

		_, ok = paths.Load(path)
		require.False(t, ok)
	})
}
