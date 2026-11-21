package util

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// #767: the bridge readiness probe reads /v1/models through the shared shape
// verdict. Only a served listing is ready; a 200 error envelope, an empty list,
// or a body whose key is not exactly "data" is not.
func TestListsModelAnswersFromTheShapeVerdictIssue767(t *testing.T) {
	requireLoopback(t)
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"served listing", `{"data":[{"id":"m"}]}`, true},
		{"numeric metadata beyond float64", `{"data":[{"id":"m","provider_metric":1e400}]}`, true},
		{"empty listing", `{"data":[]}`, false},
		{"200 error envelope", `{"error":"upstream down"}`, false},
		{"capitalised key is not a listing", `{"Data":[{"id":"m"}]}`, false},
		{"not JSON", `upstream down`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if got := listsModel(u.Port(), "k"); got != tc.want {
				t.Errorf("listsModel(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
