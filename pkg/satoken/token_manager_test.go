// Copyright 2024 The Carvel Authors.
// SPDX-License-Identifier: Apache-2.0

// This file is a modified version of
// https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/token/token_manager_test.go

package satoken

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testingclock "k8s.io/utils/clock/testing"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestTokenCachingAndExpiration(t *testing.T) {
	type suite struct {
		clock    *testingclock.FakeClock
		getter   *fakeTokenGetter
		reviewer *fakeTokenReviewer
		mgr      *Manager
	}

	type testCase struct {
		name string
		exp  time.Duration
		f    func(t *testing.T, s *suite)
	}

	testCases := []testCase{
		{
			name: "5 minute token not near expiring",
			exp:  time.Minute * 5,
			f: func(t *testing.T, s *suite) {
				s.clock.SetTime(s.clock.Now())

				_, err := s.mgr.GetServiceAccountToken(context.Background(), "a", "b", getTokenRequest())

				assert.NoErrorf(t, err, "unexpected error getting token")
				assert.Equal(t, s.getter.count, 1, "expected refresh to not be called, call count was %d", s.getter.count)
			},
		},
		{
			name: "rotate 2 hour token expires within the last hour",
			exp:  time.Hour * 2,
			f: func(t *testing.T, s *suite) {
				s.clock.SetTime(s.clock.Now().Add(time.Hour + time.Minute))

				_, err := s.mgr.GetServiceAccountToken(context.Background(), "a", "b", getTokenRequest())

				assert.NoErrorf(t, err, "unexpected error getting token")
				assert.Equal(t, s.getter.count, 2, "expected token to be refreshed, call count was %d", s.getter.count)
			},
		},
		{
			name: "rotate token fails, old token is still valid, doesn't error",
			exp:  time.Hour * 2,
			f: func(t *testing.T, s *suite) {
				s.clock.SetTime(s.clock.Now().Add(time.Hour + time.Minute))
				tg := &fakeTokenGetter{
					err: fmt.Errorf("err"),
				}
				s.mgr.getToken = tg.getToken
				tr, err := s.mgr.GetServiceAccountToken(context.Background(), "a", "b", getTokenRequest())

				assert.NoErrorf(t, err, "unexpected error getting token")
				assert.Equal(t, tr.Status.Token, "foo", "unexpected token")
			},
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			log := logf.Log.WithName("sa")
			clock := testingclock.NewFakeClock(time.Time{}.Add(30 * 24 * time.Hour))
			expSecs := int64(c.exp.Seconds())
			s := &suite{
				clock: clock,
				mgr:   NewManager(nil, log),
				getter: &fakeTokenGetter{
					request: &authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							ExpirationSeconds: &expSecs,
						},
						Status: authenticationv1.TokenRequestStatus{
							Token:               "foo",
							ExpirationTimestamp: metav1.Time{Time: clock.Now().Add(c.exp)},
						},
					},
				},
				reviewer: &fakeTokenReviewer{
					review: &authenticationv1.TokenReview{
						Spec: authenticationv1.TokenReviewSpec{
							Token: "foo",
						},
						Status: authenticationv1.TokenReviewStatus{
							Authenticated: true,
						},
					},
				},
			}
			s.mgr.getToken = s.getter.getToken
			s.mgr.reviewToken = s.reviewer.reviewToken
			s.mgr.clock = s.clock

			_, err := s.mgr.GetServiceAccountToken(context.Background(), "a", "b", getTokenRequest())
			assert.NoErrorf(t, err, "unexpected error getting token")
			assert.Equal(t, s.getter.count, 1, "unexpected client call, call count was %d", s.getter.count)

			_, err = s.mgr.GetServiceAccountToken(context.Background(), "a", "b", getTokenRequest())
			assert.NoErrorf(t, err, "unexpected error getting token")
			assert.Equal(t, s.getter.count, 1, "expected token to be served from cache, call count was %d", s.getter.count)

			c.f(t, s)
		})
	}
}

func TestRequiresRefresh(t *testing.T) {
	start := time.Now()

	type testCase struct {
		now, exp      time.Time
		authenticated bool
		expectRefresh bool
	}

	testCases := []testCase{
		{
			now:           start.Add(1 * time.Minute),
			exp:           start.Add(maxTTL),
			authenticated: true,
			expectRefresh: false,
		},
		{
			now:           start.Add(59 * time.Minute),
			exp:           start.Add(maxTTL),
			authenticated: true,
			expectRefresh: false,
		},
		{
			now:           start.Add(61 * time.Minute),
			exp:           start.Add(maxTTL),
			authenticated: true,
			expectRefresh: true,
		},
		{
			now:           start.Add(3 * time.Hour),
			exp:           start.Add(maxTTL),
			authenticated: true,
			expectRefresh: true,
		},
		{
			now:           start.Add(1 * time.Minute),
			exp:           start.Add(maxTTL),
			authenticated: false,
			expectRefresh: true,
		},
	}

	for i, c := range testCases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			log := logf.Log.WithName("sa")
			clock := testingclock.NewFakeClock(c.now)
			secs := int64(c.exp.Sub(start).Seconds())
			tr := &authenticationv1.TokenRequest{
				Spec: authenticationv1.TokenRequestSpec{
					ExpirationSeconds: &secs,
				},
				Status: authenticationv1.TokenRequestStatus{
					ExpirationTimestamp: metav1.Time{Time: c.exp},
				},
			}
			reviewer := &fakeTokenReviewer{
				review: &authenticationv1.TokenReview{
					Spec: authenticationv1.TokenReviewSpec{
						Token: "foo",
					},
					Status: authenticationv1.TokenReviewStatus{
						Authenticated: c.authenticated,
					},
				},
			}

			mgr := NewManager(nil, log)
			mgr.clock = clock
			mgr.reviewToken = reviewer.reviewToken

			rr := mgr.requiresRefresh(context.Background(), tr)
			assert.Equal(t, rr, c.expectRefresh, "unexpected requiresRefresh result, got: %v, want: %v - %s", rr, c.expectRefresh, c)
		})
	}
}

func TestCleanup(t *testing.T) {
	type testCase struct {
		name              string
		relativeExp       time.Duration
		expectedCacheSize int
	}

	testCases := []testCase{
		{
			name:              "don't cleanup unexpired tokens",
			relativeExp:       -1 * time.Hour,
			expectedCacheSize: 0,
		},
		{
			name:              "cleanup expired tokens",
			relativeExp:       time.Hour,
			expectedCacheSize: 1,
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			log := logf.Log.WithName("sa")
			clock := testingclock.NewFakeClock(time.Time{}.Add(24 * time.Hour))
			mgr := NewManager(nil, log)
			mgr.clock = clock

			mgr.set("key", &authenticationv1.TokenRequest{
				Status: authenticationv1.TokenRequestStatus{
					ExpirationTimestamp: metav1.Time{Time: mgr.clock.Now().Add(c.relativeExp)},
				},
			})
			mgr.cleanup()

			assert.Equal(t, len(mgr.cache), c.expectedCacheSize, "unexpected number of cache entries after cleanup, got: %d, want: %d", len(mgr.cache), c.expectedCacheSize)
		})
	}
}

type fakeTokenGetter struct {
	count   int
	request *authenticationv1.TokenRequest
	err     error
}

func (ftg *fakeTokenGetter) getToken(_ context.Context, _, _ string, _ *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
	ftg.count++
	return ftg.request, ftg.err
}

type fakeTokenReviewer struct {
	count  int
	review *authenticationv1.TokenReview
	err    error
}

func (ftr *fakeTokenReviewer) reviewToken(_ context.Context, _ *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
	ftr.count++
	return ftr.review, ftr.err
}

func getTokenRequest() *authenticationv1.TokenRequest {
	expiration := int64(2000)
	return &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{"foo1", "foo2"},
			ExpirationSeconds: &expiration,
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				Kind: "pod",
				Name: "foo-pod",
				UID:  "foo-uid",
			},
		},
	}
}
