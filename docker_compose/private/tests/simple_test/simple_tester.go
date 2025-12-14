package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const expectedResponse = `<!DOCTYPE html>
<html>
<head>
<title>Welcome to nginx!</title>
<style>
html { color-scheme: light dark; }
body { width: 35em; margin: 0 auto;
font-family: Tahoma, Verdana, Arial, sans-serif; }
</style>
</head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and
working. Further configuration is required.</p>

<p>For online documentation and support please refer to
<a href="http://nginx.org/">nginx.org</a>.<br/>
Commercial support is available at
<a href="http://nginx.com/">nginx.com</a>.</p>

<p><em>Thank you for using nginx.</em></p>
</body>
</html>
`

func main() {
	var host string
	flag.StringVar(&host, "host", "", "Host to fetch from (e.g., http://service_a:8080)")
	flag.Parse()

	if host == "" {
		fmt.Fprintf(os.Stderr, "Error: -host flag is required\n")
		flag.Usage()
		os.Exit(1)
	}

	// Ensure host starts with http:// or https://
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}

	// Perform HTTP GET request
	resp, err := http.Get(host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching from %s: %v\n", host, err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: received status code %d, expected %d\n", resp.StatusCode, http.StatusOK)
		os.Exit(1)
	}

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading response body: %v\n", err)
		os.Exit(1)
	}

	// Assert response matches expected string
	bodyStr := strings.TrimSpace(string(body))
	if bodyStr != strings.TrimSpace(expectedResponse) {
		fmt.Fprintf(os.Stderr, "Error: response body does not match expected '%s'\n", bodyStr)
		os.Exit(1)
	}

	fmt.Printf("Successfully fetched from %s, response matches expected string\n", host)
	os.Exit(0)
}
