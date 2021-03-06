// Package di is a dependency injection library for Go.
package di

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

var errorReftype = reflect.TypeOf((*error)(nil)).Elem()

// Injector implements the dependency injection.
type Injector struct {
	running uint32

	mu        sync.RWMutex
	providers []*provider
	deps      dependencies

	pendingMu        sync.Mutex
	pendingProviders []interface{}

	runner Runner
	logger Logger
	dones  providerDones
}

// New create a injector instance.
func New() *Injector {
	return &Injector{
		deps: make(dependencies),
	}
}

func NewAndParseEnv(prefix string) *Injector {
	inj := New()

	if os.Getenv(prefix+"DI_SYNC") == "true" {
		inj.UseRunner(syncRunner{})
	}
	if os.Getenv(prefix+"DI_LOG") == "true" {
		inj.UseLogger(DefaultLogger{})
	}
	return inj
}

func (j *Injector) UseRunner(r Runner) *Injector {
	j.runner = r
	return j
}

func (j *Injector) UseLogger(l Logger) *Injector {
	j.logger = l
	return j
}

func (j *Injector) analyseStructure(t reflect.Type, provider *provider) ([]*dependency, *structure) {
	s := &structure{
		Type: t,
	}

	l := t.NumField()
	deps := make([]*dependency, 0, l)
	for i, l := 0, t.NumField(); i < l; i++ {
		ft := t.Field(i)
		tag := ft.Tag.Get("dep")
		if tag == "-" {
			continue
		}
		n := tag
		if n == "" {
			n = ft.Name
		}
		if n == "" {
			continue
		}
		d := &dependency{
			Type:     ft.Type,
			Var:      n,
			Provider: provider,
		}
		deps = append(deps, d)
		s.fields = append(s.fields, structureField{
			fieldIndex: i,
			dependency: d,
		})
	}
	return deps, s
}

func (j *Injector) analyseFunc(name string, t reflect.Type, v reflect.Value) (*provider, error) {
	var p provider
	p.errorResolver.index = -1
	p.name = name
	if p.name == "" {
		p.name = functionName(v)
	}
	p.fn = v

	l := t.NumIn()
	p.deps = make([]*dependency, 0, l)
	for i := 0; i < l; i++ {
		in := t.In(i)
		if in.Kind() == reflect.Struct && in.Name() == "" {
			ds, parser := j.analyseStructure(in, nil)
			p.deps = append(p.deps, ds...)
			p.depParsers = append(p.depParsers, parser)
		} else {
			d := &dependency{Type: in}
			p.deps = append(p.deps, d)
			p.depParsers = append(p.depParsers, d)
		}
	}

	l = t.NumOut()
	p.provides = make([]*dependency, 0, l)
	for i := 0; i < l; i++ {
		out := t.Out(i)
		if out.Kind() == reflect.Struct && out.Name() == "" {
			ds, resolver := j.analyseStructure(out, &p)
			for _, d := range ds {
				p.provides = append(p.provides, d)
			}
			p.provideResolvers = append(p.provideResolvers, resolver)
		} else if out != errorReftype {
			d := dependency{Type: out, Provider: &p}
			p.provides = append(p.provides, &d)
			p.provideResolvers = append(p.provideResolvers, &d)
		} else {
			if p.errorResolver.index >= 0 {
				return nil, fmt.Errorf("provider returned more than one error: %s", p.name)
			}
			p.errorResolver.index = i
		}
	}
	return &p, nil
}

func (j *Injector) analyseProvider(opt optionValue) (*provider, error) {
	v := opt.Value
	t := v.Type()
	if opt.Type != nil {
		if !t.AssignableTo(opt.Type) {
			return nil, fmt.Errorf("incompatible type: %s to %s", t, opt.Type)
		}
		t = opt.Type
	}
	p := &provider{
		errorResolver: errorResolver{index: -1},
	}
	k := v.Kind()
	switch {
	case k == reflect.Func && !opt.FuncObj:
		fp, err := j.analyseFunc(opt.Name, t, v)
		if err != nil {
			return nil, err
		}
		p = fp
	case k == reflect.Struct && (opt.Decomposable || t.Name() == ""):
		ds, resolver := j.analyseStructure(t, p)
		for i, d := range ds {
			d.Val = v.Field(resolver.fields[i].fieldIndex)
		}
		p.provides = append(p.provides, ds...)
	default:
		p.provides = append(p.provides, &dependency{
			Type:     t,
			Var:      opt.Name,
			Val:      v,
			Provider: p,
		})
	}
	return p, nil
}

func (j *Injector) hasConflict(mods []*dependency, mod *dependency) (string, bool) {
	for _, m := range mods {
		if mod.Var == m.Var {
			return m.Provider.name, true
		}
	}
	return "", false
}

func (j *Injector) registerProvider(p *provider) error {
	for i := range p.provides {
		mod := p.provides[i]
		mods := j.deps[mod.Type]
		if name, conflicted := j.hasConflict(mods, mod); conflicted {
			return fmt.Errorf("provider conflicted: %s, %s, %s", name, p.name, mod.Type.String())
		}
		mods = append(mods, mod)
		if j.deps == nil {
			j.deps = make(dependencies)
		}
		j.deps[mod.Type] = mods
	}
	j.providers = append(j.providers, p)
	return nil
}

func (j *Injector) provideVal(v optionValue) error {
	p, err := j.analyseProvider(v)
	if err != nil {
		return err
	}
	return j.registerProvider(p)
}

func (j *Injector) provide(v ...interface{}) error {
	for _, arg := range v {
		o := parseOptionValue(arg)
		if o.MethodsPattern != "" {
			methods, err := j.parseMethods(o.Value, o.MethodsPattern)
			if err != nil {
				return err
			}
			for _, m := range methods {
				err = j.provideVal(m)
				if err != nil {
					return err
				}
			}
		} else {
			err := j.provideVal(o)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (j *Injector) parseMethods(refv reflect.Value, pattern string) ([]optionValue, error) {
	matcher, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	var (
		providers []optionValue
		reft      = refv.Type()
		l         = refv.Type().NumMethod()
	)
	for i := 0; i < l; i++ {
		m := reft.Method(i)
		if matcher.MatchString(m.Name) {
			providers = append(providers, optionValue{
				Name:  functionName(m.Func),
				Value: refv.Method(i),
			})
		}
	}
	return providers, nil
}

func (j *Injector) clearPendingProviders(v []interface{}) []interface{} {
	j.pendingMu.Lock()
	v = append(v, j.pendingProviders...)
	j.pendingProviders = nil
	j.pendingMu.Unlock()
	return v
}

// Provide provide value/function as providers, providers will be append to pending providers when injector is running,
// and executed at next cycle, it's helpful for unavoidable dependencies.
//
// * Value is static dependency value, it will be decomposed only if it's a structure, and it's anonymous
// or wrapped by OptDecompose.
//
// * Function is runnable provider, it depends on parameters and provide return values, empty parameters or providers is
// allowed. Parameters and return values follow the same rules with static value. And function can return at most one error to indicate
// the runtime error.
//
// Available option functions: all of OptDecompose, OptNamed, OptMethods, OptFuncObj.
func (j *Injector) Provide(v ...interface{}) error {
	if atomic.LoadUint32(&j.running) == 0 {
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.provide(j.clearPendingProviders(v)...)
	}

	j.pendingMu.Lock()
	j.pendingProviders = append(j.pendingProviders, v...)
	j.pendingMu.Unlock()
	return nil
}

func (j *Injector) runProvider(p *provider) error {
	if !p.fn.IsValid() {
		return nil
	}

	in := make([]reflect.Value, 0, len(p.depParsers))
	for _, dp := range p.depParsers {
		v, err := dp.Parse(j.deps)
		if err != nil {
			return err
		}
		in = append(in, v)
	}
	out := p.fn.Call(in)
	out, reterr := p.errorResolver.Resolve(out)
	if reterr != nil {
		return reterr
	}
	for i := range out {
		err := p.provideResolvers[i].Resolve(j.deps, out[i])
		if err != nil {
			return err
		}
	}
	return nil
}

func (j *Injector) checkAllDeps() error {
	var errs providerErrors
	for _, p := range j.providers {
		for _, dep := range p.deps {
			if j.deps.match(dep) == nil {
				errs.Append(p.name, fmt.Errorf("dependency not found: %s", dep.String()))
			}
		}
	}
	return errs.ToError()
}

// Run build a priority queue by the dependency graph, and execute each provider function, the error will
// be returned for any providers.
// Before it finished, all new providers will be marked as pending state, and be execute in next cycle.
func (j *Injector) Run() error {
	if !atomic.CompareAndSwapUint32(&j.running, 0, 1) {
		return errors.New("dependencies is already running")
	}

	runner := j.runner
	if runner == nil {
		runner = SyncRunner()
	}
	logger := j.logger
	if logger == nil {
		logger = nopLogger{}
	}
	j.mu.Lock()
	defer func() {
		j.mu.Unlock()
		atomic.StoreUint32(&j.running, 0)
	}()

	for {
		err := j.checkAllDeps()
		if err != nil {
			return err
		}

		queue, err := newQueue(j.providers, j.deps, &j.dones)
		if err != nil {
			return err
		}

		for _, n := range queue {
			p := n.provider
			err = runner.run(j, p, func() error {
				begin := time.Now()
				if p.name != "" {
					logger.Begin(p.name, begin)
				}
				err = j.runProvider(p)
				if err == nil {
					end := time.Now()
					if p.name != "" {
						logger.End(p.name, end, end.Sub(begin))
					}
					j.dones.markDone(p)
				}
				return err
			})
			if err != nil {
				return err
			}
		}
		err = runner.waitDone()
		if err != nil {
			return err
		}

		providers := j.clearPendingProviders(nil)
		if len(providers) == 0 {
			break
		}
		err = j.provide(providers...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (j *Injector) inject(v interface{}) error {
	o := parseOptionValue(v)
	if o.Value.Kind() != reflect.Ptr {
		return fmt.Errorf("destination must be pointer")
	}
	o.Value = o.Value.Elem()
	dep := dependency{
		Type: o.Value.Type(),
		Var:  o.Name,
	}
	mod := j.deps.match(&dep)
	if mod != nil {
		return dep.Inject(o.Value, j.deps)
	}
	if o.Value.Kind() != reflect.Struct || (dep.Type.Name() != "" && !o.Decomposable) {
		return dep.notExistError("")
	}
	_, r := j.analyseStructure(dep.Type, nil)
	return r.Inject(o.Value, j.deps)
}

// Inject inject all resolved dependency values to destination pointers, it should be called
// after running the injector.
// Available option functions: all of OptDecompose, OptNamed.
func (j *Injector) Inject(v ...interface{}) error {
	j.mu.RLock()
	defer j.mu.RUnlock()
	for _, p := range v {
		err := j.inject(p)
		if err != nil {
			return err
		}
	}
	return nil
}
