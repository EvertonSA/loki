package analyze

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logfmt/logfmt"
)

type ctxKey int

const analyzeKey ctxKey = 0

type Context struct {
	countIn  atomic.Int64 `json:"countIn,omitempty"`
	countOut atomic.Int64 `json:"countOut,omitempty"`
	duration atomic.Int64 `json:"duration,omitempty"`

	name        string `json:"name,omitempty"`
	description string `json:"description,omitempty"`
	index       int    `json:"index,omitempty"`

	childContexts []*Context `json:"children,omitempty"`
}

func (ctx *Context) AddChild(child *Context) {
	ctx.childContexts = append(ctx.childContexts, child)
}

// walks the contexts children recursively and adds them
// so that hopefully things are nested properly and we can
// print them nicely
func (ctx *Context) AddChildRecursively(child *Context) {
	newCtx := Context{
		countIn:     child.countIn,
		countOut:    child.countOut,
		name:        child.name,
		description: child.description,
		duration:    child.duration,
	}
	for _, c := range child.childContexts {
		newCtx.AddChildRecursively(c)
	}
	ctx.AddChild(&newCtx)
}

func (ctx *Context) GetChild(index int) *Context {
	if len(ctx.childContexts) > index {
		return ctx.childContexts[index]
	}
	if index == -1 {
		return ctx.childContexts[len(ctx.childContexts)-1]
	}
	return nil
}

func (ctx *Context) GetCounts() (int64, int64) {
	return ctx.countIn.Load(), ctx.countOut.Load()
}

func (ctx *Context) Observe(d time.Duration, match bool) {
	ctx.IncCounts(match)
	//ctx.parent.IncCounts(match)
	ctx.duration.Add(d.Nanoseconds())
}

func (ctx *Context) IncCounts(match bool) {
	ctx.countIn.Add(1)
	if match {
		ctx.countOut.Add(1)
	}
}

func (ctx *Context) String() string {
	if ctx == nil {
		return "nil"
	}
	sb := new(strings.Builder)
	ctx.stringNested(sb, 0)
	return sb.String()
}

func (ctx *Context) baseString(sb *strings.Builder) {
	sb.WriteString("AnalyzeContext")
	sb.WriteString("{")
	logfmt.NewEncoder(sb).EncodeKeyvals(
		"name", ctx.name,
		"desc", ctx.description,
		"in", ctx.countIn.Load(),
		"out", ctx.countOut.Load(),
		"duration", time.Duration(ctx.duration.Load()),
	)
	sb.WriteString("}")
}

func (ctx *Context) stringNested(sb *strings.Builder, level int) {
	for i := 0; i < level; i++ {
		sb.WriteString("\t")
	}
	ctx.baseString(sb)
	sb.WriteString("\n")
	for _, child := range ctx.childContexts {
		child.stringNested(sb, level+1)
	}
}

func (ctx *Context) Reset() {
	if ctx == nil {
		return
	}
	ctx.countIn.Store(0)
	ctx.countOut.Store(0)
	ctx.duration.Store(0)
	for idx := range ctx.childContexts {
		ctx.childContexts[idx].Reset()
	}
}

func (ctx *Context) Set(d time.Duration, in, out int64) {
	ctx.duration.Store(d.Nanoseconds())
	ctx.countIn.Store(in)
	ctx.countOut.Store(out)
}

// update recursively walks a context and updates countIn and countOut
// countIn should be the countIn for the total nested child[0] and
// countOut should be the countOut of the total nested child[:len(child)]
func (ctx *Context) Update() {
	l := len(ctx.childContexts)
	if l == 0 {
		return
	}
	in, out, dur := int64(0), int64(0), int64(0)

	// Because of the way we've written the current structure we have to
	// special case "multiple children which don't have children themselves"
	// these are the pipeline stages within the logs portion of a query
	if l >= 1 && len(ctx.childContexts[0].childContexts) == 0 {
		//d := time.Second
		in = ctx.childContexts[0].countIn.Load()
		out = ctx.childContexts[l-1].countOut.Load()
		for _, c := range ctx.childContexts {
			dur += c.duration.Load()
		}
		ctx.countIn.Store(in)
		ctx.countOut.Store(out)
		ctx.duration.Store(dur)
		return
	}

	// here we would have an execution stages which has multiple children,
	// which themselves have at least one child
	// OR
	// an execution stage with a single child which also has at least one child
	for _, c := range ctx.childContexts {
		c.Update()
		in += c.countIn.Load()
		out += c.countOut.Load()
		dur += c.duration.Load()
	}
	ctx.countIn.Store(in)
	ctx.countOut.Store(out)
	ctx.duration.Store(dur)
}

func (ctx *Context) SetDescription(d string) {
	ctx.description = d
}

func (ctx *Context) ToProto() *RemoteContext {
	children := make([]*RemoteContext, len(ctx.childContexts))
	for i, c := range ctx.childContexts {
		children[i] = c.ToProto()
	}
	return &RemoteContext{
		CountIn:     ctx.countIn.Load(),
		CountOut:    ctx.countOut.Load(),
		Duration:    ctx.duration.Load(),
		Index:       int32(ctx.index),
		Description: ctx.description,
		Name:        ctx.name,
		Children:    children,
	}
}

func New(name, description string, index int, size int) *Context {
	return &Context{
		name:          name,
		index:         index,
		description:   description,
		childContexts: make([]*Context, 0, size),
	}
}

func (a *Context) Merge(b *Context) {
	if b == nil {
		return
	}
	for idx := 0; idx < len(b.childContexts); idx++ {
		aChild := a.GetChild(idx)
		if aChild == nil {
			a.AddChild(b.GetChild(idx))
		} else {
			a.GetChild(idx).Merge(b.GetChild(idx))
		}
	}
	a.name = b.name
	a.description = b.description
	a.index = b.index
	a.countIn.Add(b.countIn.Load())
	a.countOut.Add(b.countOut.Load())
	a.duration.Add(b.duration.Load())
}

func FromProto(c *RemoteContext) *Context {
	if c == nil {
		return nil
	}
	children := make([]*Context, len(c.Children))
	for i, child := range c.Children {
		children[i] = FromProto(child)
	}
	var countIn, countOut, duration atomic.Int64
	countIn.Store(c.CountIn)
	countOut.Store(c.CountOut)
	duration.Store(c.Duration)

	return &Context{
		countIn:       countIn,
		countOut:      countOut,
		duration:      duration,
		name:          c.Name,
		description:   c.Description,
		index:         int(c.Index),
		childContexts: children,
	}
}

func NewContext(ctx context.Context, name, description string) (*Context, context.Context) {
	existing := FromContext(ctx)
	if existing != nil {
		return existing, ctx
	}
	if name == "" {
		name = "root"
	}
	c := New(name, description, 0, 2)
	return c, context.WithValue(ctx, analyzeKey, c)
}

func FromContext(ctx context.Context) *Context {
	v, _ := ctx.Value(analyzeKey).(*Context)
	return v
}
