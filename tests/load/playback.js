import http from 'k6/http';
import { check, sleep } from 'k6';
export const options = {
  vus: Number(__ENV.VUS || 100), duration: __ENV.DURATION || '2m',
  discardResponseBodies: true,
  systemTags: ['method','status','name','scenario'],
  thresholds: { http_req_failed: ['rate<0.01'], checks: ['rate>0.99'] },
};
export default function () {
  if (!__ENV.RELAY_URL) throw new Error('Set RELAY_URL to a full authenticated stream URL');
  const r = http.get(__ENV.RELAY_URL, { headers: { Range: 'bytes=0-1048575' }, tags: { name: 'media-range' } });
  check(r, { '206 response': r => r.status === 206, 'range length': r => r.headers['Content-Length'] === '1048576' });
  sleep(0.2);
}
