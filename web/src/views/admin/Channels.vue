<script setup lang="ts">
import { ref, reactive, onMounted, computed } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import * as channelsApi from '@/api/channels'
import { useUserStore } from '@/stores/user'

const userStore = useUserStore()
const canWrite = computed(() => userStore.hasPerm('channel:write'))

const loading = ref(false)
const rows = ref<channelsApi.Channel[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)

async function load() {
  loading.value = true
  try {
    const d = await channelsApi.listChannels({ page: page.value, page_size: pageSize.value })
    rows.value = d.list
    total.value = d.total
  } finally { loading.value = false }
}

// ---------- 渠道增删改 ----------
type EditState = 'create' | 'edit'
const dlgVisible = ref(false)
const dlgState = ref<EditState>('create')
const dlgLoading = ref(false)
const formRef = ref<FormInstance>()

const emptyForm = (): channelsApi.ChannelUpsert & { id: number } => ({
  id: 0,
  name: '',
  type: 'openai',
  base_url: 'https://api.openai.com',
  api_key: '',
  enabled: true,
  priority: 100,
  weight: 1,
  timeout_s: 120,
  ratio: 1.0,
  extra: '',
  remark: '',
})
const form = reactive(emptyForm())

const rules: FormRules = {
  name: [{ required: true, message: '请输入渠道名', trigger: 'blur' }],
  type: [{ required: true, message: '请选择类型', trigger: 'change' }],
  base_url: [{ required: true, message: '请输入 base_url', trigger: 'blur' }],
}

function openCreate() {
  Object.assign(form, emptyForm())
  dlgState.value = 'create'
  dlgVisible.value = true
}
function openEdit(row: channelsApi.Channel) {
  Object.assign(form, {
    id: row.id,
    name: row.name,
    type: row.type,
    base_url: row.base_url,
    api_key: '', // 不回填明文,空串表示不修改
    enabled: row.enabled,
    priority: row.priority,
    weight: row.weight,
    timeout_s: row.timeout_s,
    ratio: row.ratio,
    extra: row.extra || '',
    remark: row.remark || '',
  })
  dlgState.value = 'edit'
  dlgVisible.value = true
}

function onPickType(t: 'openai' | 'gemini') {
  form.type = t
  if (t === 'gemini') {
    if (!form.base_url || form.base_url.startsWith('https://api.openai')) {
      form.base_url = 'https://generativelanguage.googleapis.com'
    }
  } else {
    if (!form.base_url || form.base_url.includes('googleapis')) {
      form.base_url = 'https://api.openai.com'
    }
  }
}

async function submit() {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return
  const payload: channelsApi.ChannelUpsert = { ...form }
  dlgLoading.value = true
  try {
    if (dlgState.value === 'create') {
      await channelsApi.createChannel(payload)
      ElMessage.success('创建成功')
    } else {
      await channelsApi.updateChannel(form.id, payload)
      ElMessage.success('保存成功')
    }
    dlgVisible.value = false
    load()
  } finally { dlgLoading.value = false }
}

async function onDelete(row: channelsApi.Channel) {
  await ElMessageBox.confirm(
    `删除渠道 "${row.name}" 吗?相关模型映射会一并删除。`,
    '删除确认', { type: 'warning' },
  ).catch(() => null).then(async (r) => {
    if (!r) return
    await channelsApi.deleteChannel(row.id)
    ElMessage.success('已删除')
    load()
  })
}

const testingId = ref(0)
async function onTest(row: channelsApi.Channel) {
  testingId.value = row.id
  try {
    const res = await channelsApi.testChannel(row.id)
    if (res.ok) {
      ElMessage.success(`连接正常(${res.latency_ms} ms)`)
    } else {
      ElMessage.error(`连接失败:${res.error || ''}`)
    }
    load()
  } finally { testingId.value = 0 }
}

// ---------- 模型映射 ----------
const mapDlgVisible = ref(false)
const mapChannel = ref<channelsApi.Channel | null>(null)
const mappings = ref<channelsApi.Mapping[]>([])
const mapLoading = ref(false)

async function openMappings(row: channelsApi.Channel) {
  mapChannel.value = row
  mapDlgVisible.value = true
  await loadMappings()
}

async function loadMappings() {
  if (!mapChannel.value) return
  mapLoading.value = true
  try {
    const d = await channelsApi.listMappings(mapChannel.value.id)
    mappings.value = d.list || []
  } finally { mapLoading.value = false }
}

const mapForm = reactive<channelsApi.MappingUpsert>({
  local_model: '',
  upstream_model: '',
  modality: 'text',
  enabled: true,
  priority: 100,
})
const mapEditingId = ref(0)

function resetMapForm() {
  mapForm.local_model = ''
  mapForm.upstream_model = ''
  mapForm.modality = 'text'
  mapForm.enabled = true
  mapForm.priority = 100
  mapEditingId.value = 0
}

async function submitMapping() {
  if (!mapChannel.value) return
  if (!mapForm.local_model || !mapForm.upstream_model) {
    ElMessage.warning('本地模型与上游模型名都不能为空')
    return
  }
  try {
    if (mapEditingId.value > 0) {
      await channelsApi.updateMapping(mapEditingId.value, { ...mapForm })
      ElMessage.success('已更新')
    } else {
      await channelsApi.createMapping(mapChannel.value.id, { ...mapForm })
      ElMessage.success('已添加')
    }
    resetMapForm()
    await loadMappings()
  } catch (e: any) {
    ElMessage.error(e?.message || '保存失败')
  }
}

function editMapping(m: channelsApi.Mapping) {
  mapForm.local_model = m.local_model
  mapForm.upstream_model = m.upstream_model
  mapForm.modality = m.modality
  mapForm.enabled = m.enabled
  mapForm.priority = m.priority
  mapEditingId.value = m.id
}

async function delMapping(m: channelsApi.Mapping) {
  await ElMessageBox.confirm(`删除映射 ${m.local_model} → ${m.upstream_model} 吗?`,
    '删除确认', { type: 'warning' }).catch(() => null).then(async (r) => {
    if (!r) return
    await channelsApi.deleteMapping(m.id)
    await loadMappings()
  })
}

onMounted(load)
</script>

<template>
  <div class="page-container">
    <div class="card-block">
      <div class="flex-between" style="margin-bottom:12px">
        <div>
          <h2 class="page-title" style="margin:0">上游渠道</h2>
          <div style="color:var(--el-text-color-secondary);font-size:13px;margin-top:4px">
            除内置 ChatGPT 账号池外的外置 API 渠道(OpenAI / Gemini 兼容)。
            一个本地模型可映射到多个渠道以实现负载均衡与故障转移。
          </div>
        </div>
        <el-button v-if="canWrite" type="primary" :icon="Plus" @click="openCreate">
          新增渠道
        </el-button>
      </div>

      <el-table v-loading="loading" :data="rows" stripe>
        <el-table-column prop="id" label="ID" width="60" />
        <el-table-column prop="name" label="名称" min-width="150" />
        <el-table-column label="类型" width="100">
          <template #default="{ row }">
            <el-tag :type="row.type === 'openai' ? 'primary' : 'success'" size="small">
              {{ row.type }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="base_url" label="Base URL" min-width="240" show-overflow-tooltip>
          <template #default="{ row }"><code>{{ row.base_url }}</code></template>
        </el-table-column>
        <el-table-column label="API Key" width="160">
          <template #default="{ row }"><code>{{ row.api_key_masked }}</code></template>
        </el-table-column>
        <el-table-column label="优先级" width="80" prop="priority" />
        <el-table-column label="倍率" width="80">
          <template #default="{ row }">×{{ row.ratio }}</template>
        </el-table-column>
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag
              :type="row.status === 'healthy' ? 'success' : 'danger'"
              size="small"
            >
              {{ row.status === 'healthy' ? '健康' : '异常' }}
            </el-tag>
            <el-tag
              v-if="!row.enabled"
              type="info"
              size="small"
              style="margin-left:6px"
            >
              已停用
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="最近探测" min-width="200">
          <template #default="{ row }">
            <div v-if="row.last_test_at" style="font-size:12px;line-height:1.5">
              <div>{{ new Date(row.last_test_at).toLocaleString() }}</div>
              <div v-if="!row.last_test_ok" style="color:var(--el-color-danger)">
                {{ row.last_test_error || '失败' }}
              </div>
            </div>
            <span v-else style="color:var(--el-text-color-secondary)">-</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="280" fixed="right">
          <template #default="{ row }">
            <el-button
              size="small" link type="primary"
              :loading="testingId === row.id"
              @click="onTest(row)"
            >测试</el-button>
            <el-button
              v-if="canWrite"
              size="small" link type="primary"
              @click="openMappings(row)"
            >模型映射</el-button>
            <el-button
              v-if="canWrite"
              size="small" link type="primary"
              @click="openEdit(row)"
            >编辑</el-button>
            <el-button
              v-if="canWrite"
              size="small" link type="danger"
              @click="onDelete(row)"
            >删除</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div style="margin-top:12px;text-align:right">
        <el-pagination
          v-model:current-page="page"
          v-model:page-size="pageSize"
          :total="total"
          :page-sizes="[10, 20, 50]"
          layout="total, sizes, prev, pager, next"
          @current-change="load"
          @size-change="load"
          background
        />
      </div>
    </div>

    <!-- 新增 / 编辑弹窗 -->
    <el-dialog
      v-model="dlgVisible"
      :title="dlgState === 'create' ? '新增渠道' : `编辑渠道 · ${form.name}`"
      width="680px"
      destroy-on-close
    >
      <el-form ref="formRef" :model="form" :rules="rules" label-width="120px">
        <el-form-item label="名称" prop="name">
          <el-input v-model="form.name" placeholder="自定义备注名,如 OpenAI-Main" />
        </el-form-item>
        <el-form-item label="类型" prop="type">
          <el-radio-group :model-value="form.type" @update:model-value="(v: any) => onPickType(v)">
            <el-radio-button label="openai">OpenAI 兼容</el-radio-button>
            <el-radio-button label="gemini">Gemini 兼容</el-radio-button>
          </el-radio-group>
          <div class="hint">
            OpenAI 兼容:DeepSeek / Moonshot / one-api 等都是这一类。<br>
            Gemini 兼容:generativelanguage.googleapis.com 或其兼容代理。
          </div>
        </el-form-item>
        <el-form-item label="Base URL" prop="base_url">
          <el-input v-model="form.base_url" placeholder="例如 https://api.openai.com" />
          <div class="hint">
            OpenAI 类填到根,<code>/v1/...</code> 由系统自动拼接;Gemini 类填到
            <code>https://...googleapis.com</code> 即可。
          </div>
        </el-form-item>
        <el-form-item label="API Key">
          <el-input
            v-model="form.api_key"
            type="password"
            show-password
            :placeholder="dlgState === 'edit' ? '留空表示不修改' : '请输入 API Key'"
          />
        </el-form-item>
        <el-form-item label="优先级">
          <el-input-number v-model="form.priority" :min="0" :max="1000" />
          <div class="hint">越小越优先。同模型下按此值升序选取渠道。</div>
        </el-form-item>
        <el-form-item label="倍率">
          <el-input-number v-model="form.ratio" :min="0.01" :step="0.1" :precision="2" />
          <div class="hint">渠道级计费倍率。最终价格 = 模型定价 × 用户分组倍率 × 此渠道倍率。</div>
        </el-form-item>
        <el-form-item label="权重">
          <el-input-number v-model="form.weight" :min="1" />
          <div class="hint">(保留)同优先级内加权轮询,默认 1 即可。</div>
        </el-form-item>
        <el-form-item label="超时(秒)">
          <el-input-number v-model="form.timeout_s" :min="10" :max="600" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="form.remark" maxlength="255" show-word-limit />
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dlgVisible = false">取消</el-button>
        <el-button type="primary" :loading="dlgLoading" @click="submit">保存</el-button>
      </template>
    </el-dialog>

    <!-- 模型映射弹窗 -->
    <el-dialog
      v-model="mapDlgVisible"
      :title="`模型映射 · ${mapChannel?.name || ''}`"
      width="760px"
      @close="resetMapForm"
    >
      <div class="map-form-row">
        <el-input v-model="mapForm.local_model" placeholder="本地模型 slug(如 gpt-4o)" style="width:180px" />
        <el-input v-model="mapForm.upstream_model" placeholder="上游模型名(如 gpt-4o-mini / gemini-2.5-flash)" style="width:260px" />
        <el-select v-model="mapForm.modality" style="width:110px">
          <el-option label="文字" value="text" />
          <el-option label="图片" value="image" />
          <el-option label="视频" value="video" />
        </el-select>
        <el-input-number v-model="mapForm.priority" :min="0" :max="1000" style="width:100px" />
        <el-switch v-model="mapForm.enabled" />
        <el-button type="primary" :icon="Plus" @click="submitMapping">
          {{ mapEditingId ? '更新' : '添加' }}
        </el-button>
        <el-button v-if="mapEditingId" @click="resetMapForm">取消编辑</el-button>
      </div>
      <div class="hint" style="margin-bottom:8px">
        本地 slug 需先在「模型配置」中存在;同一个本地 slug 可以在多个渠道下映射,
        调度时按渠道优先级 + 映射优先级挑选。
      </div>
      <el-table v-loading="mapLoading" :data="mappings" stripe size="small">
        <el-table-column prop="id" label="ID" width="60" />
        <el-table-column prop="local_model" label="本地模型" min-width="150">
          <template #default="{ row }"><code>{{ row.local_model }}</code></template>
        </el-table-column>
        <el-table-column prop="upstream_model" label="上游模型" min-width="200">
          <template #default="{ row }"><code>{{ row.upstream_model }}</code></template>
        </el-table-column>
        <el-table-column label="模态" width="80">
          <template #default="{ row }">
            <el-tag
              size="small"
              :type="row.modality === 'image' ? 'warning' : row.modality === 'video' ? 'danger' : 'primary'"
            >{{ row.modality }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="优先级" width="80" prop="priority" />
        <el-table-column label="状态" width="80">
          <template #default="{ row }">
            <el-tag :type="row.enabled ? 'success' : 'info'" size="small">
              {{ row.enabled ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="140">
          <template #default="{ row }">
            <el-button size="small" link type="primary" @click="editMapping(row)">编辑</el-button>
            <el-button size="small" link type="danger" @click="delMapping(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-dialog>
  </div>
</template>

<style scoped>
code {
  background: #f2f3f5;
  padding: 1px 6px;
  border-radius: 4px;
  font-family: ui-monospace, Menlo, Consolas, monospace;
  font-size: 12px;
}
:global(html.dark) code { background: #1d2026; }
.hint { font-size: 12px; color: var(--el-text-color-secondary); margin-top: 4px; line-height: 1.4; }
.map-form-row {
  display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 12px; align-items: center;
}
</style>
