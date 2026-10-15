<script setup lang="ts">
import { ref, reactive, onMounted, computed } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import * as rechargeApi from '@/api/recharge'
import { formatCredit } from '@/utils/format'

const tab = ref<'packages' | 'orders'>('packages')

// ---------- packages ----------
const packages = ref<rechargeApi.Package[]>([])
const loadingPkg = ref(false)

async function loadPackages() {
  loadingPkg.value = true
  try {
    const d = await rechargeApi.adminListPackages()
    packages.value = d.items
  } finally { loadingPkg.value = false }
}

const pkgDialog = reactive({
  visible: false,
  mode: 'create' as 'create' | 'edit',
  form: {
    id: 0, name: '', price_cny: 100, credits: 1000000,
    bonus: 0, description: '', sort: 0, enabled: true,
  } as Partial<rechargeApi.Package>,
})
function openCreatePkg() {
  pkgDialog.mode = 'create'
  Object.assign(pkgDialog.form, {
    id: 0, name: '', price_cny: 100, credits: 1000000, bonus: 0,
    description: '', sort: 0, enabled: true,
  })
  pkgDialog.visible = true
}
function openEditPkg(p: rechargeApi.Package) {
  pkgDialog.mode = 'edit'
  Object.assign(pkgDialog.form, p)
  pkgDialog.visible = true
}
async function savePkg() {
  const f = pkgDialog.form
  if (!f.name || (f.price_cny ?? 0) <= 0) {
    ElMessage.warning('名称和金额不能为空')
    return
  }
  if (pkgDialog.mode === 'create') {
    await rechargeApi.adminCreatePackage(f)
    ElMessage.success('已创建')
  } else {
    await rechargeApi.adminUpdatePackage(f.id!, f)
    ElMessage.success('已保存')
  }
  pkgDialog.visible = false
  loadPackages()
}
async function deletePkg(p: rechargeApi.Package) {
  await ElMessageBox.confirm(`确认删除套餐【${p.name}】?该操作不可撤销`, '删除套餐', { type: 'warning' })
  await rechargeApi.adminDeletePackage(p.id)
  ElMessage.success('已删除')
  loadPackages()
}

// ---------- orders ----------
const orders = ref<rechargeApi.Order[]>([])
const total = ref(0)
const loadingOrd = ref(false)
const filter = reactive({
  user_id: undefined as number | undefined,
  status: '' as '' | 'pending' | 'paid' | 'cancelled' | 'expired' | 'failed',
  limit: 20,
  offset: 0,
})

async function loadOrders() {
  loadingOrd.value = true
  try {
    const d = await rechargeApi.adminListOrders({
      user_id: filter.user_id || undefined,
      status: filter.status || undefined,
      limit: filter.limit,
      offset: filter.offset,
    })
    orders.value = d.items
    total.value = d.total
  } finally { loadingOrd.value = false }
}

async function forcePaid(o: rechargeApi.Order) {
  if (o.status !== 'pending') {
    ElMessage.warning('只有 pending 状态可以手工入账')
    return
  }
  const { value: pwd } = await ElMessageBox.prompt(
    `请输入管理员密码以确认为订单 ${o.out_trade_no} 强制入账(不会调用支付收银台)。`,
    '手工入账',
    { type: 'warning', inputType: 'password', confirmButtonText: '确认入账', cancelButtonText: '取消' },
  )
  if (!pwd) return
  await rechargeApi.adminForcePaid(o.id, pwd)
  ElMessage.success('已入账')
  loadOrders()
}

const statusColor: Record<string, 'success' | 'info' | 'warning' | 'danger'> = {
  paid: 'success', pending: 'warning', cancelled: 'info', expired: 'info', failed: 'danger',
}
const statusLabel: Record<string, string> = {
  paid: '已到账', pending: '待支付', cancelled: '已取消', expired: '已超时', failed: '失败',
}

const currentPage = computed<number>({
  get() { return Math.floor(filter.offset / filter.limit) + 1 },
  set(v) { filter.offset = (v - 1) * filter.limit; loadOrders() },
})

function priceYuan(fen: number) { return (fen / 100).toFixed(2) }

onMounted(() => {
  loadPackages()
  loadOrders()
})
</script>

<template>
  <div class="page-container">
    <div class="card-block">
      <el-tabs v-model="tab">
        <el-tab-pane label="套餐管理" name="packages">
          <div class="flex-between" style="margin-bottom:10px">
            <div style="color:var(--el-text-color-secondary);font-size:13px">
              普通用户在 <b>个人中心 → 账单</b> 看到的是启用中的套餐。价格单位:分,积分单位:厘。
            </div>
            <el-button type="primary" @click="openCreatePkg">
              <el-icon><Plus /></el-icon> 新增套餐
            </el-button>
          </div>

          <el-table :data="packages" stripe v-loading="loadingPkg">
            <el-table-column prop="id" label="ID" width="70" />
            <el-table-column prop="name" label="名称" min-width="160" />
            <el-table-column label="价格" width="110">
              <template #default="{ row }">¥ {{ priceYuan(row.price_cny) }}</template>
            </el-table-column>
            <el-table-column label="基础积分" width="120">
              <template #default="{ row }">{{ formatCredit(row.credits) }}</template>
            </el-table-column>
            <el-table-column label="赠送" width="110">
              <template #default="{ row }">{{ formatCredit(row.bonus) }}</template>
            </el-table-column>
            <el-table-column prop="sort" label="排序" width="80" />
            <el-table-column label="状态" width="90">
              <template #default="{ row }">
                <el-tag :type="row.enabled ? 'success' : 'info'" size="small">
                  {{ row.enabled ? '启用' : '停用' }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="description" label="描述" show-overflow-tooltip />
            <el-table-column label="操作" width="140" fixed="right">
              <template #default="{ row }">
                <el-button size="small" link @click="openEditPkg(row)">编辑</el-button>
                <el-button size="small" link type="danger" @click="deletePkg(row)">删除</el-button>
              </template>
            </el-table-column>
          </el-table>
        </el-tab-pane>

        <el-tab-pane label="订单流水" name="orders">
          <div class="flex-wrap-gap" style="margin:10px 0">
            <el-input-number v-model="filter.user_id" :min="1" placeholder="用户 ID" style="width:140px" />
            <el-select v-model="filter.status" placeholder="状态" clearable style="width:130px">
              <el-option label="全部" value="" />
              <el-option label="待支付" value="pending" />
              <el-option label="已到账" value="paid" />
              <el-option label="已取消" value="cancelled" />
              <el-option label="已超时" value="expired" />
              <el-option label="失败" value="failed" />
            </el-select>
            <el-button type="primary" @click="() => { filter.offset = 0; loadOrders() }" :loading="loadingOrd">
              <el-icon><Search /></el-icon> 查询
            </el-button>
          </div>

          <el-table :data="orders" stripe v-loading="loadingOrd">
            <el-table-column label="订单号" min-width="180">
              <template #default="{ row }"><code style="font-size:12px">{{ row.out_trade_no }}</code></template>
            </el-table-column>
            <el-table-column prop="user_id" label="用户 ID" width="90" />
            <el-table-column label="金额" width="100">
              <template #default="{ row }">¥ {{ priceYuan(row.price_cny) }}</template>
            </el-table-column>
            <el-table-column label="积分" width="140">
              <template #default="{ row }">
                {{ formatCredit(row.credits) }} + {{ formatCredit(row.bonus) }}
              </template>
            </el-table-column>
            <el-table-column prop="pay_method" label="方式" width="90" />
            <el-table-column label="状态" width="90">
              <template #default="{ row }">
                <el-tag :type="statusColor[row.status] || 'info'" size="small">
                  {{ statusLabel[row.status] || row.status }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="支付平台单号" min-width="160">
              <template #default="{ row }">
                <span style="font-size:12px">{{ row.trade_no || '—' }}</span>
              </template>
            </el-table-column>
            <el-table-column label="支付时间" width="170">
              <template #default="{ row }">{{ row.paid_at || '—' }}</template>
            </el-table-column>
            <el-table-column label="创建时间" width="170">
              <template #default="{ row }">{{ row.created_at }}</template>
            </el-table-column>
            <el-table-column label="操作" width="130" fixed="right">
              <template #default="{ row }">
                <el-button v-if="row.status === 'pending'" size="small" link type="warning"
                           @click="forcePaid(row)">手工入账</el-button>
                <span v-else style="color:var(--el-text-color-placeholder)">—</span>
              </template>
            </el-table-column>
          </el-table>

          <el-pagination
            style="margin-top:12px"
            background
            layout="total, prev, pager, next, sizes"
            :total="total"
            v-model:current-page="currentPage"
            :page-sizes="[20, 50, 100]"
            v-model:page-size="filter.limit"
            @size-change="() => { filter.offset = 0; loadOrders() }"
          />
        </el-tab-pane>
      </el-tabs>
    </div>

    <!-- 套餐编辑弹窗 -->
    <el-dialog v-model="pkgDialog.visible"
               :title="pkgDialog.mode === 'create' ? '新增套餐' : '编辑套餐'"
               width="520px">
      <el-form label-width="110px">
        <el-form-item label="名称">
          <el-input v-model="pkgDialog.form.name" />
        </el-form-item>
        <el-form-item label="售价(分)">
          <el-input-number v-model="pkgDialog.form.price_cny" :min="1" style="width:220px" />
          <span style="margin-left:8px;color:var(--el-text-color-secondary);font-size:13px">
            = ¥ {{ ((pkgDialog.form.price_cny || 0) / 100).toFixed(2) }}
          </span>
        </el-form-item>
        <el-form-item label="基础积分(厘)">
          <el-input-number v-model="pkgDialog.form.credits" :min="0" style="width:220px" />
        </el-form-item>
        <el-form-item label="赠送积分(厘)">
          <el-input-number v-model="pkgDialog.form.bonus" :min="0" style="width:220px" />
        </el-form-item>
        <el-form-item label="排序">
          <el-input-number v-model="pkgDialog.form.sort" :min="0" />
        </el-form-item>
        <el-form-item label="状态">
          <el-switch v-model="pkgDialog.form.enabled" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="pkgDialog.form.description" type="textarea" :rows="2" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="pkgDialog.visible = false">取消</el-button>
        <el-button type="primary" @click="savePkg">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
code {
  background: #f2f3f5;
  padding: 1px 6px;
  border-radius: 4px;
  font-family: ui-monospace, Menlo, Consolas, monospace;
}
:global(html.dark) code { background: #1d2026; }
</style>
