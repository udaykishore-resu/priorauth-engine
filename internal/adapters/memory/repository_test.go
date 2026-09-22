package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/ports"
)

func intake(payer string) workflow.Intake {
	return workflow.Intake{
		Patient:  workflow.Patient{ID: "p", MemberID: "m", BirthDate: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)},
		Payer:    workflow.Payer{ID: payer},
		Provider: workflow.Provider{NPI: "1234567890"},
		Service:  workflow.Service{Code: "1", Diagnoses: []string{"A"}, ServiceDate: time.Now()},
	}
}

func create(t *testing.T, repo *Repository, id, payer string, at time.Time) *workflow.Request {
	t.Helper()
	r, evs, err := workflow.Create(id, intake(payer), at)
	require.NoError(t, err)
	envs, err := r.Emit(at, "t", evs)
	require.NoError(t, err)
	require.NoError(t, repo.Save(context.Background(), r, envs))
	return r
}

func TestRepository(t *testing.T) {
	ctx := context.Background()
	repo := NewRepository()
	require.NoError(t, repo.Ping(ctx))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	a := create(t, repo, "a", "P1", t0)
	create(t, repo, "b", "P2", t0.Add(time.Minute))

	got, err := repo.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, workflow.StateDraft, got.State)
	loaded, err := repo.Load(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, got.Version, loaded.Version)

	_, err = repo.Get(ctx, "zzz")
	require.ErrorIs(t, err, workflow.ErrNotFound)
	_, err = repo.Load(ctx, "zzz")
	require.ErrorIs(t, err, workflow.ErrNotFound)
	_, err = repo.Events(ctx, "zzz")
	require.ErrorIs(t, err, workflow.ErrNotFound)

	list, err := repo.List(ctx, ports.Filter{})
	require.NoError(t, err)
	require.Equal(t, []string{"b", "a"}, []string{list[0].ID, list[1].ID}, "newest first")
	list, err = repo.List(ctx, ports.Filter{Payer: "P1"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	list, err = repo.List(ctx, ports.Filter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, list, 1)
	list, err = repo.List(ctx, ports.Filter{State: workflow.StateApproved})
	require.NoError(t, err)
	require.Empty(t, list)

	// Optimistic concurrency: stale writer loses.
	stale, err := repo.Load(ctx, "a")
	require.NoError(t, err)
	evs, err := a.Review(workflow.Review{Action: workflow.ActionWithdraw, Actor: "x", Reason: "r"}, t0)
	require.NoError(t, err)
	envs, err := a.Emit(t0, "x", evs)
	require.NoError(t, err)
	require.NoError(t, repo.Save(ctx, a, envs))
	evs, err = stale.Review(workflow.Review{Action: workflow.ActionWithdraw, Actor: "y", Reason: "r"}, t0)
	require.NoError(t, err)
	envs, err = stale.Emit(t0, "y", evs)
	require.NoError(t, err)
	require.ErrorIs(t, repo.Save(ctx, stale, envs), ports.ErrConcurrency)
	require.NoError(t, repo.Save(ctx, stale, nil), "empty save is a no-op")

	events, err := repo.Events(ctx, "a")
	require.NoError(t, err)
	require.Len(t, events, 3)
	for _, e := range events {
		require.NotEmpty(t, e.ID)
	}
	list, err = repo.List(ctx, ports.Filter{Active: true})
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Idempotency reservation.
	id, created, err := repo.Reserve(ctx, "k", "a")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "a", id)
	id, created, err = repo.Reserve(ctx, "k", "other")
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, "a", id)

	// Publisher records.
	pub := NewPublisher()
	require.NoError(t, pub.Publish(ctx, envs))
	require.Len(t, pub.Sent(), len(envs))
	require.NoError(t, pub.Close())
}

func TestRepositoryConcurrentWriters(t *testing.T) {
	repo := NewRepository()
	t0 := time.Now()
	create(t, repo, "c", "P", t0)

	// Every writer loads the same version, then they race to commit.
	const writers = 8
	batches := make([][]workflow.Envelope, writers)
	aggs := make([]*workflow.Request, writers)
	for i := range writers {
		r, err := repo.Load(context.Background(), "c")
		require.NoError(t, err)
		evs, err := r.Review(workflow.Review{Action: workflow.ActionWithdraw, Actor: "x", Reason: "r"}, t0)
		require.NoError(t, err)
		envs, err := r.Emit(t0, "x", evs)
		require.NoError(t, err)
		aggs[i], batches[i] = r, envs
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := repo.Save(context.Background(), aggs[i], batches[i]); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else {
				require.ErrorIs(t, err, ports.ErrConcurrency)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, wins, "exactly one writer commits the same version")
}
