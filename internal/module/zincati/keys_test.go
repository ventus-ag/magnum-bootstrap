package zincati

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseFedoraMajor(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"fcos34", "ID=fedora\nVERSION_ID=34\nVARIANT=CoreOS\n", 34},
		{"fedora-quoted", "NAME=\"Fedora Linux\"\nID=fedora\nVERSION_ID=\"42\"\n", 42},
		{"dotted", "ID=fedora\nVERSION_ID=39.20240101\n", 39},
		{"ubuntu", "ID=ubuntu\nVERSION_ID=\"22.04\"\n", 0},
		{"no-version", "ID=fedora\n", 0},
		{"empty", "", 0},
	}
	for _, c := range cases {
		if got := parseFedoraMajor(c.in); got != c.want {
			t.Errorf("%s: parseFedoraMajor = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestFetchFedoraKey(t *testing.T) {
	const armored = pgpPublicKeyHeader + "\nmQINBE...\n-----END PGP PUBLIC KEY BLOCK-----\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key":
			_, _ = w.Write([]byte(armored))
		case "/notkey":
			_, _ = w.Write([]byte("<html>404 nope</html>"))
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	ctx := context.Background()

	if body, found, err := fetchFedoraKey(ctx, client, srv.URL+"/key"); err != nil || !found || string(body) != armored {
		t.Errorf("valid key: got found=%v err=%v len=%d", found, err, len(body))
	}
	if _, found, err := fetchFedoraKey(ctx, client, srv.URL+"/missing"); err != nil || found {
		t.Errorf("404: want found=false err=nil, got found=%v err=%v", found, err)
	}
	if _, found, err := fetchFedoraKey(ctx, client, srv.URL+"/notkey"); err != nil || found {
		t.Errorf("non-key body: want found=false err=nil, got found=%v err=%v", found, err)
	}
	if _, found, err := fetchFedoraKey(ctx, client, srv.URL+"/boom"); err == nil || found {
		t.Errorf("500: want err!=nil found=false, got found=%v err=%v", found, err)
	}
}
