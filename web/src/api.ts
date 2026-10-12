export const AUTH_EXPIRED_EVENT = "embyrelay-auth-expired"

export async function parseApiJSON(response: Response): Promise<any> {
  const body = await response.text()
  let data: any
  try { data = body ? JSON.parse(body) : {} }
  catch {
    if (!response.ok) return {error:`服务返回了无效响应（HTTP ${response.status}）`}
    throw new Error(`服务返回了无效响应（HTTP ${response.status}）`)
  }
  return data
}
export async function requestAPI(path: string, method = 'GET', body?: unknown) {
  const response = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: {'Content-Type':'application/json','X-Requested-With':'EmbyRelay'},
    body: body === undefined ? undefined : JSON.stringify(body)
  })
  const data = await parseApiJSON(response)
  const portal = path.startsWith('/api/v1/portal/')
  if (response.status === 401 && data.code === 'auth_expired') {
    window.dispatchEvent(new CustomEvent(AUTH_EXPIRED_EVENT, {detail:{scope:portal?'portal':'admin'}}))
  }
  return {response, data}
}
