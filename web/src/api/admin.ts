import { http } from './http'
import type { UserInfo } from './auth'

// ============ users ============
export interface AdminUser extends UserInfo {
  version?: number
  last_login_ip?: string
  updated_at?: string
  deleted_at?: string | null
}
export interface Page<T> {
  items: T[]
  total: number
  limit: number
  offset: number
}

export interface UserFilter {
  q?: string
  role?: string
  status?: string
  group_id?: number
  limit?: number
  offset?: number
}

export function listUsers(f: UserFilter = {}): Promise<Page<AdminUser>> {
  return http.get('/api/admin/users', { params: f })
}

export function getUser(id: number): Promise<AdminUser> {
  return http.get(`/api/admin/users/${id}`)
}

export interface CreateUserReq {
  email: string
  password: string
  nickname?: string
  role?: 'user' | 'admin'
  status?: 'active' | 'banned'
  group_id?: number
  initial_credits?: number
}

export function createUser(body: CreateUserReq) {
  return http.post('/api/admin/users', body)
}

export function patchUser(id: number, body: Partial<Pick<AdminUser, 'nickname' | 'role' | 'status' | 'group_id'>>) {
  return http.patch(`/api/admin/users/${id}`, body)
}

export function resetUserPassword(id: number, newPassword: string, adminPassword: string) {
  return http.post(`/api/admin/users/${id}/reset-password`,
    { new_password: newPassword, admin_password: adminPassword })
}

export function deleteUser(id: number, adminPassword: string) {
  return http.delete(`/api/admin/users/${id}`, { headers: { 'X-Admin-Confirm': adminPassword } })
}

export interface AdjustReq {
  delta: number
  remark: string
  ref_id?: string
}

export function adjustCredit(id: number, req: AdjustReq, adminPassword: string) {
  return http.post(`/api/admin/users/${id}/credits/adjust`, req, {
    headers: { 'X-Admin-Confirm': adminPassword },
  })
}

export interface CreditLog {
  id: number
  user_id: number
  key_id: number
  type: string
  amount: number
  balance_after: number
  ref_id: string
  remark: string
  created_at: string
}

export function listCreditLogs(userID: number, limit = 50, offset = 0): Promise<Page<CreditLog>> {
  return http.get(`/api/admin/users/${userID}/credit-logs`, { params: { limit, offset } })
}

// ============ credits (全局) ============

export interface CreditLogGlobal extends CreditLog {
  user_email: string
  user_nickname: string
}

export interface CreditLogFilter {
  user_id?: number
  keyword?: string
  type?: string
  sign?: '' | 'in' | 'out'
  start_at?: string
  end_at?: string
  limit?: number
  offset?: number
}

export function listCreditLogsGlobal(f: CreditLogFilter = {}): Promise<Page<CreditLogGlobal>> {
  return http.get('/api/admin/credits/logs', { params: f })
}

export interface CreditsSummary {
  in_today: number
  out_today: number
  in_7days: number
  out_7days: number
  in_total: number
  out_total: number
  total_balance: number
}

export function creditsSummary(): Promise<CreditsSummary> {
  return http.get('/api/admin/credits/summary')
}

export function adjustCreditByUser(
  req: { user_id: number; delta: number; remark: string; ref_id?: string },
  adminPassword: string,
) {
  return http.post<any, { balance_after: number; delta: number }>(
    '/api/admin/credits/adjust',
    req,
    { headers: { 'X-Admin-Confirm': adminPassword } },
  )
}

// ============ groups ============

export interface Group {
  id: number
  name: string
  ratio: number
  daily_limit_credits: number
  rpm_limit: number
  tpm_limit: number
  remark: string
  created_at?: string
  updated_at?: string
}

export function listGroups(): Promise<{ items: Group[]; total: number }> {
  return http.get('/api/admin/groups')
}

export function createGroup(g: Partial<Group>): Promise<Group> {
  return http.post('/api/admin/groups', g)
}

export function updateGroup(id: number, g: Partial<Group>): Promise<Group> {
  return http.put(`/api/admin/groups/${id}`, g)
}

export function deleteGroup(id: number): Promise<{ deleted: number }> {
  return http.delete(`/api/admin/groups/${id}`)
}

// ============ audit ============
export interface AuditLog {
  id: number
  actor_id: number
  actor_email: string
  action: string
  method: string
  path: string
  status_code: number
  ip: string
  ua: string
  target: string
  meta?: string | Record<string, unknown> | null
  created_at: string
}

export interface AuditFilter { actor_id?: number; action?: string; limit?: number; offset?: number }

export function listAudit(f: AuditFilter = {}): Promise<{ items: AuditLog[]; total: number; limit: number; offset: number }> {
  return http.get('/api/admin/audit/logs', { params: f })
}
