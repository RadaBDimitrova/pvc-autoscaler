// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package downscale

import (
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/pvc-autoscaler/internal/common"
)

func TestNewValidation(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	rec := record.NewFakeRecorder(1)
	rm := apimeta.NewDefaultRESTMapper(nil)
	sc := &fakeScales{}
	cp := &fakeCopier{}

	tests := []struct {
		name    string
		opts    []Option
		wantErr error
	}{
		{
			name:    "missing client",
			opts:    []Option{WithEventRecorder(rec), WithScaleClient(sc), WithRESTMapper(rm), WithCopier(cp)},
			wantErr: ErrNoClient,
		},
		{
			name:    "missing event recorder",
			opts:    []Option{WithClient(c), WithScaleClient(sc), WithRESTMapper(rm), WithCopier(cp)},
			wantErr: common.ErrNoEventRecorder,
		},
		{
			name:    "missing scale client",
			opts:    []Option{WithClient(c), WithEventRecorder(rec), WithRESTMapper(rm), WithCopier(cp)},
			wantErr: ErrNoScaleClient,
		},
		{
			name:    "missing REST mapper",
			opts:    []Option{WithClient(c), WithEventRecorder(rec), WithScaleClient(sc), WithCopier(cp)},
			wantErr: ErrNoRESTMapper,
		},
		{
			name:    "missing copier",
			opts:    []Option{WithClient(c), WithEventRecorder(rec), WithScaleClient(sc), WithRESTMapper(rm)},
			wantErr: ErrNoCopier,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts...); err != tt.wantErr {
				t.Fatalf("New() error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	if _, err := New(WithClient(c), WithEventRecorder(rec), WithScaleClient(sc), WithRESTMapper(rm), WithCopier(cp)); err != nil {
		t.Fatalf("New() with a valid config returned error: %v", err)
	}
}
