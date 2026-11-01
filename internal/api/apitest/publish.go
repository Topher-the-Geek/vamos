package apitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/api"
)

// testPublish checks that a publish job uploads every image, then writes every tag in every repository,
// reporting each destination; that a failed upload writes no tag; that a failure while tagging attempts every tag
// and reports which were written; and that a publication that isn't one is refused before any work.
func testPublish(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	// The registry refuses every manifest written to the repository refused, and the tag v1 written to the
	// repository untaggable, whose uploads by digest and other tags it takes.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			reference := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if strings.HasPrefix(r.URL.Path, "/v2/refused/") ||
				strings.HasPrefix(r.URL.Path, "/v2/untaggable/") && reference == "v1" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"errors": [{"code": "DENIED", "message": "writes refused"}]}`))
				return
			}
		}
		f.Registry.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	f.insecure(t, host)
	layer := runGraph(t, ex, layerGraph(t, f, ex, "published"))[0]
	other := runGraph(t, ex, layerGraph(t, f, ex, "published too"))[0]

	// Every repository gets its image under every tag.
	req := api.PublishRequest{
		Images: []api.PublishImage{
			{Result: layer, Repository: host + "/team/app"},
			{Result: layer, Repository: host + "/mirror/app"},
			{Result: other, Repository: host + "/team/other"},
		},
		Tags: []string{"v1", "latest"},
	}
	outcome := finishJob(t, ex, api.Request{Publish: &req})
	want := []api.Destination{
		{Repository: host + "/team/app", Tag: "v1", Image: layer.Image, Written: true},
		{Repository: host + "/team/app", Tag: "latest", Image: layer.Image, Written: true},
		{Repository: host + "/mirror/app", Tag: "v1", Image: layer.Image, Written: true},
		{Repository: host + "/mirror/app", Tag: "latest", Image: layer.Image, Written: true},
		{Repository: host + "/team/other", Tag: "v1", Image: other.Image, Written: true},
		{Repository: host + "/team/other", Tag: "latest", Image: other.Image, Written: true},
	}
	if outcome.Published == nil || !reflect.DeepEqual(outcome.Published.Destinations, want) {
		t.Errorf("published = %+v, want %+v", outcome.Published, want)
	}
	for _, repo := range []string{"team/app", "mirror/app", "team/other"} {
		if tags := registryTags(t, host, repo); !slices.Equal(tags, []string{"latest", "v1"}) {
			t.Errorf("%s has tags %v, want latest and v1", repo, tags)
		}
	}

	// An upload that fails writes no tag, even to the repositories uploaded to first.
	req = api.PublishRequest{
		Images: []api.PublishImage{
			{Result: layer, Repository: host + "/first/app"},
			{Result: layer, Repository: host + "/refused/app"},
		},
		Tags: []string{"v1"},
	}
	outcome = failJob(t, ex, api.Request{Publish: &req})
	if !strings.Contains(outcome.Failure.Message, host+"/refused/app") || outcome.Published != nil {
		t.Errorf("a refused upload = %+v, want a failure naming the repository, and nothing published", outcome)
	}
	if tags := registryTags(t, host, "first/app"); len(tags) != 0 {
		t.Errorf("after a failed upload, first/app has tags %v, want none", tags)
	}

	// A tag that fails is reported with those written, and every tag is attempted, in its repository and others.
	req = api.PublishRequest{
		Images: []api.PublishImage{
			{Result: layer, Repository: host + "/untaggable/app"},
			{Result: layer, Repository: host + "/tagged/app"},
		},
		Tags: []string{"v1", "latest"},
	}
	outcome = failJob(t, ex, api.Request{Publish: &req})
	if msg := outcome.Failure.Message; !strings.Contains(msg, host+"/untaggable/app:v1") || !strings.Contains(msg,
		"written: "+host+"/untaggable/app:latest, "+host+"/tagged/app:v1, "+host+"/tagged/app:latest") {
		t.Errorf("a refused tag = %q, want it named, and the tags written named too", outcome.Failure.Message)
	}
	written := []bool{false, true, true, true}
	if p := outcome.Published; p == nil || len(p.Destinations) != len(written) || p.Destinations[0].Error == "" {
		t.Errorf("published = %+v, want untaggable/app:v1 unwritten, with why, and the other tags written", p)
	} else {
		for i, d := range p.Destinations {
			if d.Written != written[i] || d.Image != layer.Image {
				t.Errorf("destination %d = %+v, want written %t, naming image %s", i, d, written[i], layer.Image)
			}
		}
	}
	if tags := registryTags(t, host, "untaggable/app"); !slices.Equal(tags, []string{"latest"}) {
		t.Errorf("untaggable/app has tags %v, want latest", tags)
	}

	// A publication must have an image and a tag, full repositories with no tag, and valid tags, each once.
	image := api.PublishImage{Result: layer, Repository: host + "/app"}
	for _, bad := range []api.PublishRequest{
		{Tags: []string{"v1"}},
		{Images: []api.PublishImage{image}},
		{Images: []api.PublishImage{image}, Tags: []string{"not a tag"}},
		{Images: []api.PublishImage{image}, Tags: []string{"v1", "v1"}},
		{Images: []api.PublishImage{{Result: layer, Repository: host + "/app:v1"}}, Tags: []string{"v1"}},
		{Images: []api.PublishImage{{Result: layer, Repository: "app"}}, Tags: []string{"v1"}},
		{Images: []api.PublishImage{image, image}, Tags: []string{"v1"}},
	} {
		if id, err := ex.Submit(ctx, api.Request{Publish: &bad}); err == nil {
			t.Errorf("Submit accepted the publication %+v as job %s", bad, id)
		}
	}
}

// failJob submits req, watches it to its end and returns its outcome, which must be a failure.
func failJob(t *testing.T, ex api.Executor, req api.Request) api.Outcome {
	t.Helper()
	ctx := t.Context()
	id, err := ex.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	watchAll(t, ex, id, 0)
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure == nil {
		t.Fatalf("Result = %+v, %v; want a failure", outcome, err)
	}
	return outcome
}

// registryTags returns the tags of the repository repo at the registry whose address is host, sorted;
// none if it has no such repository.
func registryTags(t *testing.T, host, repo string) []string {
	t.Helper()
	resp, err := http.Get("http://" + host + "/v2/" + repo + "/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	var list struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	slices.Sort(list.Tags)
	return list.Tags
}
