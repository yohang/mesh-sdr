package api_test

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/yohang/mesh-sdr/internal/http/api"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func TestReadiness(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	for _, tt := range []struct {
		err  error
		want api.GetReadinessResponseObject
	}{
		{nil, api.GetReadiness200JSONResponse{Status: api.HealthStatusOk}},
		{errors.New("down"), api.GetReadiness503JSONResponse{Status: api.HealthStatusDegraded}},
	} {
		got, err := api.NewHealthHandlers(pinger{tt.err}, logger).GetReadiness(context.Background(), api.GetReadinessRequestObject{})
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ping error %v: readiness = %#v, %v; want %#v", tt.err, got, err, tt.want)
		}
	}
}
