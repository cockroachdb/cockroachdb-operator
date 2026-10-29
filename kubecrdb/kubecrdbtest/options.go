package kubecrdbtest

type options struct {
	createRBAC    bool
	createCerts   bool
	createService bool
}

type optionFn func(*options)

func (fn optionFn) apply(o *options) { fn(o) }

type Option interface {
	apply(*options)
}

func RBAC(create bool) Option {
	return optionFn(func(o *options) { o.createRBAC = create })
}

func Certs(create bool) Option {
	return optionFn(func(o *options) { o.createCerts = create })
}

func Service(create bool) Option {
	return optionFn(func(o *options) { o.createService = create })
}
