package testmanifest

// Factory is a test helper. It makes it easy to create unique test kubernetes
// objects like tenants and pods.
type Factory struct {
	namespace string
	counter   int
}

// NewFactory creates a factory that creates kubernetes objects in the argument
// namespace.
func NewFactory(namespace string) *Factory {
	return &Factory{namespace: namespace, counter: 1}
}

// NextID returns a unique ID. It is used to provide unique object names for
// objects created by the factory.
func (f *Factory) NextID() int {
	next := f.counter
	f.counter++
	return next
}
