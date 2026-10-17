package api

import (
	"slices"

	"github.com/Topher-the-Geek/vamos/internal/tracing"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// The Clone methods return a copy of a value that shares no memory with it,
// so that an executor can hand out what it keeps, and keep what it is handed,
// and a caller that changes its copy changes nothing else, as a transport's would.

// Clone returns a copy of c that shares no memory with it.
func (c Capabilities) Clone() Capabilities {
	c.Platform = clonePlatform(c.Platform)
	c.GraphVersions = slices.Clone(c.GraphVersions)
	return c
}

// Clone returns a copy of r that shares no memory with it.
func (r Request) Clone() Request {
	if r.Graph != nil {
		g := r.Graph.Clone()
		r.Graph = &g
	}
	if r.Push != nil {
		p := r.Push.Clone()
		r.Push = &p
	}
	if r.Export != nil {
		e := r.Export.Clone()
		r.Export = &e
	}
	return r
}

// Clone returns a copy of r that shares no memory with it.
func (r PushRequest) Clone() PushRequest {
	r.Result = r.Result.Clone()
	r.Trace = clonePointer(r.Trace)
	return r
}

// Clone returns a copy of r that shares no memory with it.
func (r ExportRequest) Clone() ExportRequest {
	r.Result = r.Result.Clone()
	r.Trace = clonePointer(r.Trace)
	return r
}

// Clone returns a copy of r that shares no memory with it.
func (r GraphRequest) Clone() GraphRequest {
	r.Platform = clonePlatform(r.Platform)
	r.Trace = clonePointer(r.Trace)
	return r
}

// Clone returns a copy of e that shares no memory with it.
func (e Event) Clone() Event {
	e.Nodes = slices.Clone(e.Nodes)
	e.Data = slices.Clone(e.Data)
	if e.Spans != nil {
		spans := make([]tracing.Span, len(e.Spans))
		for i, s := range e.Spans {
			spans[i] = s.Clone()
		}
		e.Spans = spans
	}
	if e.Outcome != nil {
		o := e.Outcome.Clone()
		e.Outcome = &o
	}
	return e
}

// Clone returns a copy of o that shares no memory with it.
func (o Outcome) Clone() Outcome {
	if o.Results != nil {
		results := make([]Result, len(o.Results))
		for i, r := range o.Results {
			results[i] = r.Clone()
		}
		o.Results = results
	}
	o.Pushed = clonePointer(o.Pushed)
	if o.Exported != nil {
		e := o.Exported.Clone()
		o.Exported = &e
	}
	o.Pruned = clonePointer(o.Pruned)
	if o.Failure != nil {
		f := o.Failure.Clone()
		o.Failure = &f
	}
	return o
}

// Clone returns a copy of e that shares no memory with it.
func (e Exported) Clone() Exported {
	e.Blobs = slices.Clone(e.Blobs)
	e.Files = clonePointer(e.Files)
	return e
}

// Clone returns a copy of r that shares no memory with it.
func (r Result) Clone() Result {
	r.Index = slices.Clone(r.Index)
	r.Files = clonePointer(r.Files)
	return r
}

// Clone returns a copy of f that shares no memory with it.
func (f Failure) Clone() Failure {
	f.Nodes = slices.Clone(f.Nodes)
	f.Step = clonePointer(f.Step)
	f.Log = clonePointer(f.Log)
	return f
}

// clonePlatform returns a copy of p that shares no memory with it.
func clonePlatform(p imgspecv1.Platform) imgspecv1.Platform {
	p.OSFeatures = slices.Clone(p.OSFeatures)
	return p
}

// clonePointer returns a pointer to a copy of what p points to, or nil, for values that hold no references.
func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
