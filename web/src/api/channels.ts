import { http } from './http'

export interface Channel {
  id: number
  name: string
  type: 'openai' | 'gemini'
  base_url: string
  api_key_masked?: string
  enabled: boolean
  priority: number
  weight: number
  timeout_s: number
  ratio: number
  extra?: string
  status: 'healthy' | 'unhealthy'
  fail_count: number
  last_test_at?: string
  last_test_ok?: boolean
  last_test_error?: string
  remark: string
  created_at: string
  updated_at: string
}

export interface ChannelUpsert {
  name: string
  type: 'openai' | 'gemini'
  base_url: string
  api_key?: string
  enabled: boolean
  priority: number
  weight: number
  timeout_s: number
  ratio: number
  extra?: string
  remark?: string
}

export interface Mapping {
  id: number
  channel_id: number
  local_model: string
  upstream_model: string
  modality: 'text' | 'image' | 'video'
  enabled: boolean
  priority: number
  created_at: string
  updated_at: string
}

export interface MappingUpsert {
  local_model: string
  upstream_model: string
  modality: 'text' | 'image' | 'video'
  enabled: boolean
  priority: number
}

export function listChannels(params: { page?: number; page_size?: number } = {}):
  Promise<{ list: Channel[]; total: number; page: number; page_size: number }> {
  return http.get('/api/admin/channels', { params })
}
export function createChannel(body: ChannelUpsert): Promise<Channel> {
  return http.post('/api/admin/channels', body)
}
export function updateChannel(id: number, body: ChannelUpsert): Promise<Channel> {
  return http.patch(`/api/admin/channels/${id}`, body)
}
export function deleteChannel(id: number) {
  return http.delete(`/api/admin/channels/${id}`)
}
export function testChannel(id: number):
  Promise<{ ok: boolean; latency_ms: number; error?: string }> {
  return http.post(`/api/admin/channels/${id}/test`, {})
}

export function listMappings(channelID: number): Promise<{ list: Mapping[] }> {
  return http.get(`/api/admin/channels/${channelID}/mappings`)
}
export function createMapping(channelID: number, body: MappingUpsert): Promise<Mapping> {
  return http.post(`/api/admin/channels/${channelID}/mappings`, body)
}
export function updateMapping(mid: number, body: MappingUpsert): Promise<Mapping> {
  return http.patch(`/api/admin/channel-mappings/${mid}`, body)
}
export function deleteMapping(mid: number) {
  return http.delete(`/api/admin/channel-mappings/${mid}`)
}
