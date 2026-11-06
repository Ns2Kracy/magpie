package davsync

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// R2's bucket API is at the account root, not at an object prefix pasted
// into Endpoint. Existing objects must stay at exactly the same URLs.
func TestS3R2EndpointPathKeepsObjectsAndListsBucket(t *testing.T) {
	for _, tc := range []struct {
		path      string
		pathStyle bool
		prefix    string
	}{
		{"/bkt", false, "bkt/"},
		{"/bkt/", false, "bkt/"},
		{"/team x+y/nested", false, "team x+y/nested/"},
		{"/bkt", true, "bkt/"},
		{"/bkt/team", true, "team/bkt/"},
	} {
		t.Run(fmt.Sprintf("%s/path-style=%v", tc.path, tc.pathStyle), func(t *testing.T) {
			f, srv := newFakeS3(t)
			target, _ := url.Parse(srv.URL)
			cfg := f.config(srv)
			cfg.Endpoint = "https://acct.r2.cloudflarestorage.com" + tc.path
			cfg.PathStyle = tc.pathStyle
			cfg.Region = "us-east-1"
			key := tc.prefix + "team x+y/magpie/"
			name := "0123456789abcdef-2026-01-01.magpie-usage"
			f.store(key+file, []byte("backup"))
			f.store(key+usageFolder+"/"+name, []byte("sealed usage"))
			srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("list-type") == "2" {
					f.mu.Lock()
					defer f.mu.Unlock()
					root := "/"
					if tc.pathStyle {
						root = "/bkt"
					}
					if r.URL.Path != root {
						f.fail(w, http.StatusNotFound, "NoSuchKey")
						return
					}
					prefix := r.URL.Query().Get("prefix")
					if prefix != key+usageFolder+"/" {
						t.Errorf("listing prefix %q, want %q", prefix, key+usageFolder+"/")
					}
					if !f.signed(r, nil) {
						f.fail(w, http.StatusForbidden, "SignatureDoesNotMatch")
						return
					}
					fmt.Fprint(w, `<ListBucketResult>`)
					for k := range f.objects {
						if strings.HasPrefix(k, prefix) {
							fmt.Fprintf(w, `<Contents><Key>%s</Key><ETag>%s</ETag></Contents>`, k, f.etags[k])
						}
					}
					fmt.Fprint(w, `<IsTruncated>false</IsTruncated></ListBucketResult>`)
					return
				}
				if !tc.pathStyle {
					f.mu.Lock()
					defer f.mu.Unlock()
					if !f.signed(r, nil) {
						f.fail(w, http.StatusForbidden, "SignatureDoesNotMatch")
						return
					}
					key := strings.TrimPrefix(r.URL.Path, "/")
					data, ok := f.objects[key]
					if !ok {
						f.fail(w, http.StatusNotFound, "NoSuchKey")
						return
					}
					w.Header().Set("ETag", f.etags[key])
					w.Write(data)
					return
				}
				f.ServeHTTP(w, r)
			})
			s, err := newS3(cfg)
			if err != nil {
				t.Fatal(err)
			}
			endpoint, _ := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
			old := &s3{endpoint: endpoint, bucket: "bkt", key: "team x+y/magpie/" + file, pathStyle: tc.pathStyle}
			if s.objectURL().String() != old.objectURL().String() {
				t.Fatalf("backup moved: %s, was %s", s.objectURL(), old.objectURL())
			}
			old.key = "team x+y/magpie/" + usageFolder + "/" + name
			if s.urlOf(s.usageKey(name)).String() != old.objectURL().String() {
				t.Fatalf("usage moved: %s, was %s", s.urlOf(s.usageKey(name)), old.objectURL())
			}
			s.client = &http.Client{Transport: r2FixtureTransport{target: target, base: http.DefaultTransport}}
			got, err := s.list(context.Background())
			if err != nil || len(got) != 1 {
				t.Fatalf("usage list: %v, %v", got, err)
			}
			backup, _, err := s.get(context.Background(), version{})
			if err != nil || string(backup) != "backup" {
				t.Fatalf("backup read: %q, %v", backup, err)
			}
			data, err := s.read(context.Background(), name)
			if err != nil || string(data) != "sealed usage" {
				t.Fatalf("usage read: %q, %v", data, err)
			}
		})
	}
}

type r2FixtureTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (p r2FixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	copy.URL = &u
	copy.Host = r.URL.Host
	u.Scheme = p.target.Scheme
	u.Host = p.target.Host
	return p.base.RoundTrip(copy)
}

func TestS3R2PathStyleEndpointNamesItsBucket(t *testing.T) {
	_, err := newS3(Config{URL: "s3://bkt", Endpoint: "https://acct.r2.cloudflarestorage.com/other", PathStyle: true})
	if err == nil || !strings.Contains(err.Error(), "other, not bkt") {
		t.Fatalf("conflicting bucket in endpoint was accepted: %v", err)
	}
}

func TestS3ProxyEndpointPathStillRoutesToProxy(t *testing.T) {
	for _, pathStyle := range []bool{false, true} {
		s, err := newS3(Config{URL: "s3://bkt/team", Endpoint: "https://s3.example.com/proxy/s3", PathStyle: pathStyle})
		if err != nil {
			t.Fatal(err)
		}
		want := "https://bkt.s3.example.com/proxy/s3/"
		if pathStyle {
			want = "https://s3.example.com/proxy/s3/bkt"
		}
		if s.bucketURL().String() != want || s.key != "team/magpie/"+file {
			t.Fatalf("proxy path changed: %s, key %s", s.bucketURL(), s.key)
		}
	}
}
