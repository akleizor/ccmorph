import http.server
import socketserver

import pytest

# Fake gateways take the adapter's 8 concurrent probes; the stdlib default backlog
# of 5 makes macOS reset the overflow, which the adapter reads as a transport
# error and the tests saw as a randomly missing model.
socketserver.TCPServer.request_queue_size = 128


# HTTPServer.server_bind resolves its own address with socket.getfqdn, a reverse
# DNS lookup of 127.0.0.1 that can stall for tens of seconds on macOS; every fake
# gateway here binds loopback and never uses the name. Subprocess fakes the tests
# write out cannot see this file, so each of those carries FAKE_SERVER_NO_DNS.
def _bind_without_reverse_dns(self):
    socketserver.TCPServer.server_bind(self)
    self.server_name, self.server_port = self.server_address[:2]


http.server.HTTPServer.server_bind = _bind_without_reverse_dns


@pytest.hookimpl(tryfirst=True)
def pytest_cmdline_main(config):
    # Every core by default when pytest-xdist is installed; `-n0` or a plain
    # environment without xdist runs serially, as before.
    if (config.pluginmanager.hasplugin("xdist") and not hasattr(config, "workerinput")
            and config.option.numprocesses is None):
        config.option.numprocesses = "auto"
        config.option.dist = "worksteal"
