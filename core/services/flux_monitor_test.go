package services_test

import (
	"chainlink/core/internal/cltest"
	"chainlink/core/internal/mocks"
	"chainlink/core/services"
	"chainlink/core/store/models"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestConcreteFluxMonitor_AddJobHappy(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	job := cltest.NewJobWithFluxMonitorInitiator()
	runManager := new(mocks.RunManager)
	started := make(chan struct{})

	dc := new(mocks.DeviationChecker)
	dc.On("Initialize", mock.Anything).Return(nil)
	dc.On("Start").Return().Run(func(mock.Arguments) {
		started <- struct{}{}
	})

	checkerFactory := new(mocks.DeviationCheckerFactory)
	checkerFactory.On("New", mock.Anything, job.Initiators[0], runManager).Return(dc, nil)
	fm := services.NewFluxMonitor(store, runManager)
	services.ExportedSetCheckerFactory(fm, checkerFactory)
	require.NoError(t, fm.Connect(nil))
	defer fm.Disconnect()

	require.NoError(t, fm.AddJob(job))

	cltest.CallbackOrTimeout(t, "deviation checker started", func() {
		<-started
	})
	checkerFactory.AssertExpectations(t)
	dc.AssertExpectations(t)
}

func TestConcreteFluxMonitor_AddJobError(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	job := cltest.NewJobWithFluxMonitorInitiator()
	runManager := new(mocks.RunManager)
	dc := new(mocks.DeviationChecker)
	dc.On("Initialize", mock.Anything).Return(errors.New("deliberate test error"))
	checkerFactory := new(mocks.DeviationCheckerFactory)
	checkerFactory.On("New", mock.Anything, job.Initiators[0], runManager).Return(dc, nil)
	fm := services.NewFluxMonitor(store, runManager)
	services.ExportedSetCheckerFactory(fm, checkerFactory)
	require.NoError(t, fm.Connect(nil))
	defer fm.Disconnect()

	require.Error(t, fm.AddJob(job))
	checkerFactory.AssertExpectations(t)
	dc.AssertExpectations(t)
}

func TestConcreteFluxMonitor_AddJobDisconnected(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	job := cltest.NewJobWithFluxMonitorInitiator()
	runManager := new(mocks.RunManager)
	checkerFactory := new(mocks.DeviationCheckerFactory)
	fm := services.NewFluxMonitor(store, runManager)
	services.ExportedSetCheckerFactory(fm, checkerFactory)

	require.Error(t, fm.AddJob(job))
}

func TestConcreteFluxMonitor_ConnectStartsExistingJobs(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	job := cltest.NewJobWithFluxMonitorInitiator()
	require.NoError(t, store.CreateJob(&job))

	job, err := store.FindJob(job.ID) // Update job from db to get matching coerced time values (UpdatedAt)
	require.NoError(t, err)

	runManager := new(mocks.RunManager)
	started := make(chan struct{})

	dc := new(mocks.DeviationChecker)
	dc.On("Initialize", mock.Anything).Return(nil)
	dc.On("Start").Return().Run(func(mock.Arguments) {
		started <- struct{}{}
	})

	checkerFactory := new(mocks.DeviationCheckerFactory)
	checkerFactory.On("New", mock.Anything, job.Initiators[0], runManager).Return(dc, nil)
	fm := services.NewFluxMonitor(store, runManager)
	services.ExportedSetCheckerFactory(fm, checkerFactory)
	require.NoError(t, fm.Connect(nil))
	cltest.CallbackOrTimeout(t, "deviation checker started", func() {
		<-started
	})
	checkerFactory.AssertExpectations(t)
	dc.AssertExpectations(t)
}

func TestPollingDeviationChecker_Happy(t *testing.T) {
	priceResponse := `{"data":{"result": 102}}` // enough to trigger a deviation
	mockServer, serverAssert := cltest.NewHTTPMockServer(t, http.StatusOK, "POST", priceResponse)

	job := cltest.NewJobWithFluxMonitorInitiator()
	initr := job.Initiators[0]
	initr.ID = 1
	initr.InitiatorParams.Feeds = models.Feeds([]string{mockServer.URL})

	rm := new(mocks.RunManager)
	run := cltest.NewJobRun(job)
	data := models.JSON{Result: gjson.Parse(`{"result":"102"}`)}
	rm.On("Create", job.ID, &initr, &data, mock.Anything, mock.Anything).
		Return(&run, nil)

	checker, err := services.NewPollingDeviationChecker(context.Background(), initr, rm, time.Second)
	require.NoError(t, err)
	assert.Equal(t, decimal.NewFromInt(0), checker.CurrentPrice())

	ethClient := new(mocks.Client)
	ethClient.On("GetAggregatorPrice", initr.InitiatorParams.Address, initr.InitiatorParams.Precision).
		Return(decimal.NewFromInt(100), nil)

	require.NoError(t, checker.Initialize(ethClient))
	ethClient.AssertExpectations(t)
	assert.Equal(t, decimal.NewFromInt(100), checker.CurrentPrice())

	assert.NoError(t, checker.Poll())

	serverAssert()
	rm.AssertExpectations(t)
	assert.Equal(t, decimal.NewFromInt(102), checker.CurrentPrice())
}

func TestPollingDeviationChecker_InitializeError(t *testing.T) {
	rm := new(mocks.RunManager)
	job := cltest.NewJobWithFluxMonitorInitiator()
	initr := job.Initiators[0]
	initr.ID = 1

	ethClient := new(mocks.Client)
	ethClient.On("GetAggregatorPrice", initr.InitiatorParams.Address, initr.InitiatorParams.Precision).
		Return(decimal.NewFromInt(0), errors.New("deliberate test error"))

	checker, err := services.NewPollingDeviationChecker(context.Background(), initr, rm, time.Second)
	require.NoError(t, err)
	require.Error(t, checker.Initialize(ethClient))
}

func TestPollingDeviationChecker_NoDeviationLoopsThenCancels(t *testing.T) {
	// 1. Set up external adapter to mark when polled
	priceResponse := `{"data":{"result": 100}}`
	polled := make(chan struct{})
	mockServer, serverAssert := cltest.NewHTTPMockServer(t, http.StatusOK, "POST", priceResponse, func(http.Header, string) {
		polled <- struct{}{}
	})
	defer serverAssert()

	// 2. Prepare initialization to 100, which matches external adapter, so no deviation
	job := cltest.NewJobWithFluxMonitorInitiator()
	initr := job.Initiators[0]
	initr.ID = 1
	initr.InitiatorParams.Feeds = models.Feeds([]string{mockServer.URL})

	ethClient := new(mocks.Client)
	ethClient.On("GetAggregatorPrice", initr.InitiatorParams.Address, initr.InitiatorParams.Precision).
		Return(decimal.NewFromInt(100), nil)

	// 3. Start() with no delay to speed up test and polling.
	rm := new(mocks.RunManager)
	parentCtx, cancel := context.WithCancel(context.Background())
	checker, err := services.NewPollingDeviationChecker(parentCtx, initr, rm, 0)
	require.NoError(t, err)
	require.NoError(t, checker.Initialize(ethClient))

	done := make(chan struct{})
	go func() {
		checker.Start() // Start() polling and delaying until cancel() from parent ctx.
		done <- struct{}{}
	}()

	// 4. Check if Polled
	cltest.CallbackOrTimeout(t, "start repeatedly polls external adapter", func() {
		<-polled // launched at the beginning of Start before delay
		<-polled // need two hits
	})

	// 5. Cancel parent context and ensure Start() stops.
	cancel()
	cltest.CallbackOrTimeout(t, "Start() unblocks and is done", func() {
		<-done
	})
}