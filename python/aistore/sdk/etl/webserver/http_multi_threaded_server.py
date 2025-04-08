#
# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
#

import os
from http.server import HTTPServer, BaseHTTPRequestHandler
from socketserver import ThreadingMixIn
from typing import Type, Tuple
import signal
import threading
from urllib.parse import unquote

import requests

from aistore.sdk.etl.webserver.base_etl_server import ETLServer
from aistore.sdk.const import HEADER_CONTENT_LENGTH, HEADER_CONTENT_TYPE, STATUS_OK


class ThreadedHTTPServer(ThreadingMixIn, HTTPServer):
    """
    Multi-threaded HTTP server that delegates ETL logic to a provided ETLServer instance.
    """

    def __init__(
        self,
        server_address: Tuple[str, int],
        handler_class: Type[BaseHTTPRequestHandler],
        etl_server: ETLServer,
    ):
        super().__init__(server_address, handler_class)
        self.etl_server = etl_server


class HTTPMultiThreadedServer(ETLServer):
    """
    Multi-threaded HTTP server implementation for ETL payload transformation.
    Handles GET and PUT requests via a request handler class.
    """

    def __init__(self, host: str = "0.0.0.0", port: int = 80):
        super().__init__()
        self.host = host
        self.port = port

    # pylint: disable=invalid-name
    class RequestHandler(BaseHTTPRequestHandler):
        """
        Request handler for the ETL HTTP server.
        Forwards GET/PUT requests to the AIS target and applies transformation logic.
        """

        def _set_headers(self, status_code: int = STATUS_OK, length: int = 0):
            self.send_response(status_code)
            mime_type = self.server.etl_server.get_mime_type()
            self.send_header(HEADER_CONTENT_TYPE, mime_type)
            self.send_header(HEADER_CONTENT_LENGTH, str(length))
            self.end_headers()

        def log_request(self, code="-", size="-"):
            # Suppress default request logging (or override as needed)
            pass

        def _get_fqn_content(self, path: str) -> bytes:
            """
            Parses and safely reads a file when using arg_type == 'fqn'.
            """
            decoded_path = unquote(path)
            safe_path = os.path.normpath(os.path.join("/", decoded_path.lstrip("/")))

            self.server.etl_server.logger.debug("Reading local file: %s", safe_path)

            with open(safe_path, "rb") as f:
                return f.read()

        def do_GET(self):
            """
            Handle GET requests by forwarding them to the AIS target or reading from FQN,
            applying the ETL transformation and returning the result.
            """
            path = self.path
            logger = self.server.etl_server.logger
            logger.debug("Received GET request for path: %s", path)

            # Health check
            if path == "/health":
                resp = b"Running"
                self._set_headers(length=len(resp))
                self.wfile.write(resp)
                return

            try:
                if self.server.etl_server.arg_type == "fqn":
                    content = self._get_fqn_content(path)
                else:
                    target_url = f"{self.server.etl_server.host_target}{path}"
                    logger.debug("Forwarding GET to AIS target: %s", target_url)

                    resp = requests.get(target_url, timeout=None)

                    if resp.status_code != STATUS_OK:
                        logger.warning(
                            "Failed to fetch from target %s (status %d)",
                            target_url,
                            resp.status_code,
                        )
                        self.send_error(
                            resp.status_code, "Failed to retrieve data from target"
                        )
                        return

                    content = resp.content

                transformed = self.server.etl_server.transform(content, path)
                self._set_headers(length=len(transformed))
                self.wfile.write(transformed)

            except FileNotFoundError:
                logger.error("File not found: %s", path)
                self.send_error(404, f"Local file not found: {path}")

            except requests.RequestException as e:
                logger.error("Request to AIS target failed: %s", str(e))
                self.send_error(500, f"Error contacting AIS target: {e}")

            except Exception as e:
                logger.exception("Unexpected error processing GET request")
                self.send_error(500, f"Unhandled error: {e}")

        def do_PUT(self):
            """
            Handle PUT requests by transforming the incoming data and responding with the transformed data.
            """
            logger = self.server.etl_server.logger
            path = self.path
            logger.debug("Received PUT request for path: %s", path)

            try:
                if self.server.etl_server.arg_type == "fqn":
                    content = self._get_fqn_content(path)
                else:
                    content_length = int(self.headers.get(HEADER_CONTENT_LENGTH, 0))
                    content = self.rfile.read(content_length)
                transformed = self.server.etl_server.transform(content, path)
                self._set_headers(length=len(transformed))
                self.wfile.write(transformed)

            except FileNotFoundError:
                logger.error("File not found: %s", path)
                self.send_error(404, f"Local file not found: {path}")

            except Exception as e:
                logger.error("Error processing PUT request: %s", str(e))
                self.send_error(500, "Internal error during transformation")

    def start(self):
        """
        Starts the HTTP server and gracefully handles SIGTERM/SIGINT for Kubernetes.
        """
        server = ThreadedHTTPServer(
            (self.host, self.port), self.RequestHandler, etl_server=self
        )
        self.logger.info(
            "HTTP Multi-threaded server running at %s:%s", self.host, self.port
        )

        # Register shutdown handler
        def shutdown_handler(signum, _frame):
            self.logger.info("Received signal %s: shutting down.", signum)
            threading.Thread(target=server.shutdown).start()

        signal.signal(signal.SIGTERM, shutdown_handler)
        signal.signal(signal.SIGINT, shutdown_handler)

        try:
            server.serve_forever()
        finally:
            server.server_close()
            self.logger.info("Server shutdown complete.")
