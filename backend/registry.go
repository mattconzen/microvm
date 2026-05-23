package backend

import "fmt"

type Registry struct {
	backends map[string]Backend
}

func NewRegistry() *Registry {
	return &Registry{backends: map[string]Backend{}}
}

func (r *Registry) Register(b Backend) {
	r.backends[b.Name()] = b
}

func (r *Registry) Get(name string) (Backend, error) {
	b, ok := r.backends[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (registered: %v)", name, r.Names())
	}
	return b, nil
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.backends))
	for n := range r.backends {
		out = append(out, n)
	}
	return out
}
