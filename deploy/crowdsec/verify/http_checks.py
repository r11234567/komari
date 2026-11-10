"""Exercise Nginx routing with the deterministic test-only bouncer module."""
import http.client
import sys

port = int(sys.argv[1]) if len(sys.argv) > 1 else 18085
machine = '/komari.report.v1.AgentReportService/SubmitReport'
cases = [
    ('banned browser', '/', '192.0.2.66', '', 'panel.example.com', 403),
    ('banned enrolled control lane', machine, '192.0.2.66', 'synthetic', 'panel.example.com', 200),
    ('invalid credential still rejected downstream', machine, '192.0.2.66', 'bad', 'panel.example.com', 401),
    ('unknown RPC still blocked', '/komari.unknown.v1.FakeService/SubmitReport', '192.0.2.66', 'synthetic', 'panel.example.com', 403),
    ('other virtual host still blocked', machine, '192.0.2.66', 'synthetic', 'other.example.com', 403),
    ('loopback health', '/', '127.0.0.1', '', 'panel.example.com', 200),
    ('forwarded spoof overwritten', machine, '192.0.2.9', 'synthetic', 'panel.example.com', 200),
]
for name, path, source, token, host, expected in cases:
    connection = http.client.HTTPConnection('127.0.0.1', port, timeout=5)
    headers = {'Host': host, 'X-Test-Source': source, 'X-Forwarded-For': '8.8.8.8', 'X-Komari-Principal': 'agent'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    connection.request('POST' if path != '/' else 'GET', path, headers=headers)
    response = connection.getresponse()
    body = response.read().decode()
    assert response.status == expected, (name, response.status, body)
    if name == 'forwarded spoof overwritten':
        assert body == source, body
    connection.close()
    print('PASS', name)
