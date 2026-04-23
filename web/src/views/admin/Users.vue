<script setup lang="ts">
import { ref, reactive, onMounted, computed } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import * as adminApi from '@/api/admin'
import { formatCredit, formatDateTime } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()

const loading = ref(false)
const filter = reactive<adminApi.UserFilter>({
  q: '', role: '', status: '', group_id: undefined,
  limit: 20, offset: 0,
})
const total = ref(0)
const rows = ref<adminApi.AdminUser[]>([])
const groups = ref<adminApi.Group[]>([])

async function fetchList() {
  loading.value = true
  try {
    const data = await adminApi.listUsers({
      ...filter,
      group_id: filter.group_id || undefined,
    })
    rows.value = data.items
    total.value = data.total
  } finally {
    loading.value = false
  }
}

async function fetchGroups() {
  try {
    const g = await adminApi.listGroups()
    groups.value = g.items
  } catch { /* noop */ }
}

function groupName(id: number) {
  return groups.value.find((g) => g.id === id)?.name || `#${id}`
}

function resetFilter() {
  filter.q = ''
  filter.role = ''
  filter.status = ''
  filter.group_id = undefined
  filter.offset = 0
  fetchList()
}

// ---- 添加用户 ----
const createDlg = ref(false)
const createForm = reactive({
  email: '',
  password: '',
  nickname: '',
  role: 'user' as 'user' | 'admin',
  status: 'active' as 'active' | 'banned',
  group_id: 1,
  initial_credits: 0,
})
function openCreate() {
  createForm.email = ''
  createForm.password = ''
  createForm.nickname = ''
  createForm.role = 'user'
  createForm.status = 'active'
  createForm.group_id = groups.value[0]?.id || 1
  createForm.initial_credits = 0
  createDlg.value = true
}
async function onCreateSubmit() {
  const email = createForm.email.trim()
  if (!email) return ElMessage.warning('请输入邮箱')
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) return ElMessage.warning('邮箱格式不正确')
  if (createForm.password.length < 6) return ElMessage.warning('密码至少 6 位')
  await adminApi.createUser({
    email,
    password: createForm.password,
    nickname: createForm.nickname.trim(),
    role: createForm.role,
    status: createForm.status,
    group_id: createForm.group_id,
    initial_credits: createForm.initial_credits > 0 ? createForm.initial_credits : undefined,
  })
  ElMessage.success('用户创建成功')
  createDlg.value = false
  filter.offset = 0
  fetchList()
}

// ---- 编辑 ----
const editDlg = ref(false)
const editingRow = ref<adminApi.AdminUser | null>(null)
const editForm = reactive({ nickname: '', role: '', status: '', group_id: 1 })

function openEdit(row: adminApi.AdminUser) {
  editingRow.value = row
  editForm.nickname = row.nickname
  editForm.role = row.role
  editForm.status = row.status
  editForm.group_id = row.group_id
  editDlg.value = true
}
async function onSaveEdit() {
  if (!editingRow.value) return
  await adminApi.patchUser(editingRow.value.id, { ...editForm })
  ElMessage.success('保存成功')
  editDlg.value = false
  fetchList()
}

// ---- 重置密码 ----
const pwdDlg = ref(false)
const pwdForm = reactive({ uid: 0, newPwd: '', adminPwd: '' })
function openReset(row: adminApi.AdminUser) {
  pwdForm.uid = row.id
  pwdForm.newPwd = ''
  pwdForm.adminPwd = ''
  pwdDlg.value = true
}
async function onResetSubmit() {
  if (pwdForm.newPwd.length < 6) return ElMessage.warning('新密码至少 6 位')
  if (!pwdForm.adminPwd) return ElMessage.warning('请输入管理员密码')
  await adminApi.resetUserPassword(pwdForm.uid, pwdForm.newPwd, pwdForm.adminPwd)
  ElMessage.success('已重置')
  pwdDlg.value = false
}

// ---- 调账 ----
const adjustDlg = ref(false)
const adjustForm = reactive({ uid: 0, delta: 0, remark: '', ref_id: '', adminPwd: '' })
function openAdjust(row: adminApi.AdminUser) {
  adjustForm.uid = row.id
  adjustForm.delta = 0
  adjustForm.remark = ''
  adjustForm.ref_id = ''
  adjustForm.adminPwd = ''
  adjustDlg.value = true
}
async function onAdjustSubmit() {
  if (!adjustForm.delta) return ElMessage.warning('金额不能为 0')
  if (!adjustForm.remark) return ElMessage.warning('请填备注')
  if (!adjustForm.adminPwd) return ElMessage.warning('请输入管理员密码')
  await adminApi.adjustCredit(adjustForm.uid, {
    delta: adjustForm.delta,
    remark: adjustForm.remark,
    ref_id: adjustForm.ref_id,
  }, adjustForm.adminPwd)
  ElMessage.success('调账成功')
  adjustDlg.value = false
  fetchList()
}

// ---- 删除 ----
async function onDelete(row: adminApi.AdminUser) {
  if (row.id === store.user?.id) return ElMessage.warning('不能删除自己')
  const { value: pwd } = await ElMessageBox.prompt(
    `确认删除用户 ${row.email}?此操作会将账号标记为已删除并封禁。请输入你的管理员密码:`,
    '删除用户',
    {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      inputType: 'password',
      type: 'warning',
      inputPlaceholder: '管理员密码',
    },
  ).catch(() => ({ value: null }))
  if (!pwd) return
  await adminApi.deleteUser(row.id, pwd)
  ElMessage.success('已删除')
  fetchList()
}

// ---- 流水 ----
const logsDlg = ref(false)
const logs = ref<adminApi.CreditLog[]>([])
const logLoading = ref(false)
async function openLogs(row: adminApi.AdminUser) {
  logsDlg.value = true
  logLoading.value = true
  try {
    const data = await adminApi.listCreditLogs(row.id, 100, 0)
    logs.value = data.items
  } finally {
    logLoading.value = false
  }
}

const canEdit = computed(() => store.hasPerm('user:write'))
const canCredit = computed(() => store.hasPerm('user:credit'))

onMounted(() => { fetchGroups(); fetchList() })
</script>

<template>
  <div class="page-container">
    <div class="card-block">
      <h2 class="page-title" style="margin:0">用户管理</h2>
      <div style="color:var(--el-text-color-secondary);font-size:13px;margin:4px 0 14px">
        维护平台用户:角色分配、状态冻结、分组倍率、重置密码与积分调整入口。
      </div>

      <el-form inline class="flex-wrap-gap" @submit.prevent="fetchList">
        <el-input v-model="filter.q" placeholder="邮箱 / 昵称" clearable style="width:220px" />
        <el-select v-model="filter.role" placeholder="角色" clearable style="width:120px">
          <el-option label="普通用户" value="user" />
          <el-option label="管理员" value="admin" />
        </el-select>
        <el-select v-model="filter.status" placeholder="状态" clearable style="width:120px">
          <el-option label="正常" value="active" />
          <el-option label="已封禁" value="banned" />
        </el-select>
        <el-select v-model="filter.group_id" placeholder="分组" clearable style="width:150px">
          <el-option v-for="g in groups" :key="g.id" :label="g.name" :value="g.id" />
        </el-select>
        <el-button type="primary" @click="fetchList"><el-icon><Search /></el-icon> 查询</el-button>
        <el-button @click="resetFilter">重置</el-button>
        <el-button type="success" @click="openCreate" :disabled="!canEdit">
          <el-icon><Plus /></el-icon> 添加用户
        </el-button>
      </el-form>

      <el-table v-loading="loading" :data="rows" stripe style="margin-top:12px">
        <el-table-column prop="id" label="ID" width="70" />
        <el-table-column prop="email" label="邮箱" min-width="200" />
        <el-table-column prop="nickname" label="昵称" min-width="120" />
        <el-table-column label="角色" width="90">
          <template #default="{ row }">
            <el-tag :type="row.role === 'admin' ? 'warning' : 'info'" size="small">
              {{ row.role === 'admin' ? '管理员' : '普通' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="row.status === 'active' ? 'success' : 'info'" size="small">
              {{ row.status === 'active' ? '正常' : '封禁' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="分组" width="110">
          <template #default="{ row }">{{ groupName(row.group_id) }}</template>
        </el-table-column>
        <el-table-column label="余额 / 冻结" min-width="160">
          <template #default="{ row }">
            <div>{{ formatCredit(row.credit_balance) }}</div>
            <div style="font-size:12px;color:var(--el-text-color-secondary)">
              冻结 {{ formatCredit(row.credit_frozen) }}
            </div>
          </template>
        </el-table-column>
        <el-table-column label="最近登录" min-width="160">
          <template #default="{ row }">
            <div style="font-size:12px">
              <div>{{ formatDateTime(row.last_login_at) }}</div>
              <div style="color:var(--el-text-color-secondary)">{{ row.last_login_ip || '-' }}</div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="300" fixed="right">
          <template #default="{ row }">
            <el-button size="small" link type="primary" @click="openEdit(row)" :disabled="!canEdit">编辑</el-button>
            <el-button size="small" link type="primary" @click="openReset(row)" :disabled="!canEdit">重置密码</el-button>
            <el-button size="small" link type="warning" @click="openAdjust(row)" :disabled="!canCredit">调账</el-button>
            <el-button size="small" link @click="openLogs(row)">流水</el-button>
            <el-button size="small" link type="danger" @click="onDelete(row)" :disabled="!canEdit">删除</el-button>
          </template>
        </el-table-column>
      </el-table>

      <el-pagination
        style="margin-top:16px;justify-content:flex-end;display:flex"
        :current-page="Math.floor((filter.offset || 0) / (filter.limit || 20)) + 1"
        @current-change="(p: number) => { filter.offset = (p - 1) * (filter.limit || 20); fetchList() }"
        :page-size="filter.limit"
        @size-change="(s: number) => { filter.limit = s; filter.offset = 0; fetchList() }"
        :total="total"
        :page-sizes="[20, 50, 100]"
        layout="total, sizes, prev, pager, next"
      />
    </div>

    <!-- 添加用户 -->
    <el-dialog v-model="createDlg" title="添加用户" width="500px">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom:12px"
                title="此入口由管理员直接建号,不受『开放注册开关』和『邮箱域名白名单』限制;新账号立即可用。" />
      <el-form :model="createForm" label-width="100px">
        <el-form-item label="邮箱" required>
          <el-input v-model="createForm.email" placeholder="user@example.com" clearable />
        </el-form-item>
        <el-form-item label="初始密码" required>
          <el-input v-model="createForm.password" type="password" show-password placeholder="≥ 6 位,可让用户首次登录后自行修改" />
        </el-form-item>
        <el-form-item label="昵称">
          <el-input v-model="createForm.nickname" maxlength="40" show-word-limit />
        </el-form-item>
        <el-form-item label="角色">
          <el-select v-model="createForm.role" style="width:100%">
            <el-option label="普通用户" value="user" />
            <el-option label="管理员" value="admin" />
          </el-select>
        </el-form-item>
        <el-form-item label="状态">
          <el-select v-model="createForm.status" style="width:100%">
            <el-option label="正常" value="active" />
            <el-option label="封禁" value="banned" />
          </el-select>
        </el-form-item>
        <el-form-item label="分组">
          <el-select v-model="createForm.group_id" style="width:100%">
            <el-option v-for="g in groups" :key="g.id" :label="g.name" :value="g.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="初始积分">
          <el-input-number v-model="createForm.initial_credits" :min="0" :step="10000" style="width:100%" />
          <div style="font-size:12px;color:var(--el-text-color-secondary);margin-top:4px">
            单位:厘(1 积分 = 10000 厘)。>0 时会走正规入账,写 credit_logs。
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createDlg = false">取消</el-button>
        <el-button type="primary" @click="onCreateSubmit">创建</el-button>
      </template>
    </el-dialog>

    <!-- 编辑 -->
    <el-dialog v-model="editDlg" title="编辑用户" width="480px">
      <el-form :model="editForm" label-width="90px">
        <el-form-item label="昵称"><el-input v-model="editForm.nickname" /></el-form-item>
        <el-form-item label="角色">
          <el-select v-model="editForm.role" style="width:100%">
            <el-option label="普通用户" value="user" />
            <el-option label="管理员" value="admin" />
          </el-select>
        </el-form-item>
        <el-form-item label="状态">
          <el-select v-model="editForm.status" style="width:100%">
            <el-option label="正常" value="active" />
            <el-option label="封禁" value="banned" />
          </el-select>
        </el-form-item>
        <el-form-item label="分组">
          <el-select v-model="editForm.group_id" style="width:100%">
            <el-option v-for="g in groups" :key="g.id" :label="g.name" :value="g.id" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="editDlg = false">取消</el-button>
        <el-button type="primary" @click="onSaveEdit">保存</el-button>
      </template>
    </el-dialog>

    <!-- 重置密码 -->
    <el-dialog v-model="pwdDlg" title="重置密码" width="420px">
      <el-alert type="warning" :closable="false" show-icon style="margin-bottom:12px"
                title="该操作会强制写入新密码,旧登录 token 仍短时可用,请提醒用户改密并退出重登。" />
      <el-form label-width="110px">
        <el-form-item label="新密码">
          <el-input v-model="pwdForm.newPwd" type="password" show-password placeholder="≥ 6 位" />
        </el-form-item>
        <el-form-item label="管理员密码">
          <el-input v-model="pwdForm.adminPwd" type="password" show-password placeholder="二次确认" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="pwdDlg = false">取消</el-button>
        <el-button type="danger" @click="onResetSubmit">确认重置</el-button>
      </template>
    </el-dialog>

    <!-- 调账 -->
    <el-dialog v-model="adjustDlg" title="积分调账" width="440px">
      <el-alert type="warning" :closable="false" show-icon style="margin-bottom:12px"
                title="正数为加款,负数为扣款;扣款不会把余额扣成负数。单位:厘(1 积分 = 10000 厘)" />
      <el-form label-width="110px">
        <el-form-item label="金额(厘)">
          <el-input-number v-model="adjustForm.delta" :step="10000" style="width:100%" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="adjustForm.remark" maxlength="200" show-word-limit />
        </el-form-item>
        <el-form-item label="关联单号">
          <el-input v-model="adjustForm.ref_id" placeholder="选填,订单号/工单号" />
        </el-form-item>
        <el-form-item label="管理员密码">
          <el-input v-model="adjustForm.adminPwd" type="password" show-password />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="adjustDlg = false">取消</el-button>
        <el-button type="warning" @click="onAdjustSubmit">确认</el-button>
      </template>
    </el-dialog>

    <!-- 流水 -->
    <el-dialog v-model="logsDlg" title="积分流水" width="820px">
      <el-table v-loading="logLoading" :data="logs" max-height="480" size="small">
        <el-table-column prop="id" label="ID" width="80" />
        <el-table-column prop="type" label="类型" width="120" />
        <el-table-column label="金额" width="140">
          <template #default="{ row }">
            <span :style="{color: row.amount >= 0 ? '#67c23a' : '#f56c6c'}">
              {{ row.amount >= 0 ? '+' : '' }}{{ formatCredit(row.amount) }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="余额" width="110">
          <template #default="{ row }">{{ formatCredit(row.balance_after) }}</template>
        </el-table-column>
        <el-table-column prop="ref_id" label="关联" min-width="120" />
        <el-table-column prop="remark" label="备注" min-width="180" show-overflow-tooltip />
        <el-table-column prop="created_at" label="时间" width="170" />
      </el-table>
    </el-dialog>
  </div>
</template>
