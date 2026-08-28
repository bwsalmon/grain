package gemini

// NewForTest returns a Framework backed by an arbitrary generator
// implementing this package's own contentGenerator method set, for
// callers outside gemini that need to script the tool-call loop without a
// live API key -- v2/gitproxy's live_test.go, notably, which drives a
// full clone/commit/push through a real git proxy this way. Kept separate
// from gemini_test.go's own fakeGenerator because that type is internal
// to this package's tests; this is the seam an external package uses
// instead of reimplementing newFramework's unexported constructor.
func NewForTest(generator contentGenerator, opts ...Option) *Framework {
	return newFramework(generator, opts...)
}
