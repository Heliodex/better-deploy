package wasihttp

const (
	// HeaderBasePath is the application base path.
	HeaderBasePath = "spin-base-path"
	// HeaderComponentRoot is the component route pattern matched, excluding any wildcard indicator.
	HeaderComponentRoot = "spin-component-route"
	// HeaderFullURL is the full URL of the request, including full host and scheme information.
	HeaderFullURL = "spin-full-url"
	// HeaderMatchedRoute is the part of the request path that was matched by the route, including the base and wildcard indicator if present.
	HeaderMatchedRoute = "spin-matched-route"
	// HeaderPathInfo is the request path relative to the component route, including any base.
	HeaderPathInfo = "spin-path-info"
	// HeaderRawComponentRoot is the component route pattern matched, as written in the component manifest. It excludes the base but includes the wildcard indicator if present.
	HeaderRawComponentRoot = "spin-raw-component-route"
	// HeaderClientAddr is the client address for the request.
	HeaderClientAddr = "spin-client-addr"
	// HeaderRequestID carries a request identifier across proxied and outbound requests.
	HeaderRequestID = "X-Request-ID"
)
