"""Integration test for docker-compose services.

This test fetches the nginx welcome page from a docker-compose service
and verifies the response matches the expected content.
"""

import os
import sys

import pytest
import requests

EXPECTED_RESPONSE = """\
<!DOCTYPE html>
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
"""


def test_nginx_response() -> None:
    """Test that nginx returns the expected welcome page."""
    host = os.environ.get("SERVICE_HOST")
    assert host, "SERVICE_HOST environment variable is required"

    # Ensure host has http:// prefix
    if not host.startswith("http://") and not host.startswith("https://"):
        url = f"http://{host}"
    else:
        url = host

    print(f"Fetching from {url}...")

    # Perform HTTP GET request
    response = requests.get(url)

    # Check status code
    assert (
        response.status_code == 200
    ), f"Expected status 200, got {response.status_code}"

    # Compare response with expected content
    assert response.text.strip() == EXPECTED_RESPONSE.strip(), (
        f"Response body does not match expected.\n"
        f"Got:\n{response.text.strip()}\n\n"
        f"Expected:\n{EXPECTED_RESPONSE.strip()}"
    )

    print(f"Successfully fetched from {url}, response matches expected content")


# Important for being able to run pytest.
if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
