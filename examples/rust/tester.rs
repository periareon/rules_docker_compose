//! Integration test for docker-compose services.
//!
//! This test fetches the nginx welcome page from a docker-compose service
//! and verifies the response matches the expected content.

use std::env;

const EXPECTED_RESPONSE: &str = r#"<!DOCTYPE html>
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
</html>"#;

#[test]
fn test_nginx_response() {
    let host =
        env::var("SERVICE_HOST").expect("SERVICE_HOST environment variable is required");

    // Ensure host has http:// prefix
    let url = if host.starts_with("http://") || host.starts_with("https://") {
        host
    } else {
        format!("http://{}", host)
    };

    println!("Fetching from {}...", url);

    // Use blocking reqwest client
    let response = reqwest::blocking::get(&url).unwrap();

    // Check status code
    let status = response.status();
    assert!(status.is_success(), "Expected status 200, got {}", status);

    // Read response body
    let body = response.text().unwrap();

    // Compare response with expected content
    assert_eq!(body.trim(), EXPECTED_RESPONSE.trim(), "Response body does not match expected");
}
