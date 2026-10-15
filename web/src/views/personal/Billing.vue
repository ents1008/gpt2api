<script setup lang="ts">
import { ref, reactive, onMounted, computed } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import * as rechargeApi from '@/api/recharge'
import { formatCredit } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const userStore = useUserStore()
const packages = ref<rechargeApi.Package[]>([])
const channelEnabled = ref(false)
const orders = ref<rechargeApi.Order[]>([])
const total = ref(0)
const paging = reactive({ limit: 10, offset: 0, status: '' as '' | 'pending' | 'paid' | 'cancelled' | 'expired' })
const loadingPkg = ref(false)
const loadingOrder = ref(false)

async function loadPackages() {
  loadingPkg.value = true
  try {
    const d = await rechargeApi.listMyPackages()
    packages.value = d.items
    channelEnabled.value = d.enabled
  } finally { loadingPkg.value = false }
}

async function loadOrders() {
  loadingOrder.value = true
  try {
    const d = await rechargeApi.listMyOrders({
      limit: paging.limit,
      offset: paging.offset,
      status: paging.status || undefined,
    })
    orders.value = d.items
    total.value = d.total
  } finally { loadingOrder.value = false }
}

/**
 * 下单 -> 打开 pay_url(支付收银台)
 * 返回后用户点"刷新"即可拉到最新订单状态(支付回调已异步把 pending -> paid)。
 */
async function buy(pkg: rechargeApi.Package, payType?: string) {
  if (!channelEnabled.value) {
    ElMessage.warning('支付通道未配置,请联系管理员')
    return
  }
  try {
    const order = await rechargeApi.createOrder(pkg.id, payType)
    if (!order.pay_url) {
      ElMessage.error('支付链接生成失败')
      return
    }
    window.open(order.pay_url, '_blank', 'noopener,noreferrer')
    ElMessageBox.alert(
      `订单号:${order.out_trade_no}\n\n支付完成后请返回本页并点击"刷新"按钮查看到账状态。`,
      '已跳转支付',
      { confirmButtonText: '去刷新订单', callback: () => { paging.offset = 0; loadOrders() } },
    )
  } catch (e: any) {
    // Axios 拦截器已经 Toast 过,这里额外兜底
    if (e?.message) ElMessage.error(e.message)
  }
}

async function cancel(o: rechargeApi.Order) {
  await ElMessageBox.confirm(`确认取消订单 ${o.out_trade_no}?`, '取消订单', { type: 'warning' })
  await rechargeApi.cancelMyOrder(o.id)
  ElMessage.success('已取消')
  loadOrders()
}

const statusColor: Record<string, 'success' | 'info' | 'warning' | 'danger'> = {
  paid: 'success', pending: 'warning', cancelled: 'info', expired: 'info', failed: 'danger',
}
const statusLabel: Record<string, string> = {
  paid: '已到账', pending: '待支付', cancelled: '已取消', expired: '已超时', failed: '失败',
}

const currentPage = computed<number>({
  get() { return Math.floor(paging.offset / paging.limit) + 1 },
  set(v) { paging.offset = (v - 1) * paging.limit; loadOrders() },
})

function priceYuan(fen: number) { return (fen / 100).toFixed(2) }
function openPayUrl(url: string) { window.open(url, '_blank', 'noopener,noreferrer') }

onMounted(() => {
  loadPackages()
  loadOrders()
})
</script>

<template>
  <div class="page-container">
    <!-- 当前余额 -->
    <div class="card-block">
      <div class="flex-between">
        <div>
          <div style="font-size:13px;color:var(--el-text-color-secondary)">当前可用积分</div>
          <div style="font-size:32px;font-weight:700;color:#409eff;margin-top:6px">
            {{ formatCredit(userStore.user?.credit_balance) }}
          </div>
          <div style="font-size:12px;color:var(--el-text-color-secondary)">
            冻结 {{ formatCredit(userStore.user?.credit_frozen) }} 积分
          </div>
        </div>
        <el-button @click="userStore.fetchMe()" size="small">
          <el-icon><Refresh /></el-icon> 刷新余额
        </el-button>
      </div>
    </div>

    <!-- 套餐 -->
    <div class="card-block">
      <div class="flex-between" style="margin-bottom:12px">
        <h3 style="margin:0;font-size:15px">选择充值套餐</h3>
        <el-tag v-if="!channelEnabled" type="warning" size="small">
          支付通道未配置
        </el-tag>
      </div>
      <el-empty v-if="!loadingPkg && packages.length === 0" description="暂无可用套餐" />
      <el-row :gutter="16" v-loading="loadingPkg">
        <el-col v-for="p in packages" :key="p.id" :md="8" :sm="12" :xs="24" style="margin-bottom:16px">
          <el-card shadow="hover" class="pkg-card">
            <div class="pkg-name">{{ p.name }}</div>
            <div class="pkg-price">¥ <span>{{ priceYuan(p.price_cny) }}</span></div>
            <div class="pkg-credit">
              到账 <b>{{ formatCredit(p.credits) }}</b> 积分
              <span v-if="p.bonus > 0" class="bonus">+赠送 {{ formatCredit(p.bonus) }}</span>
            </div>
            <div class="pkg-desc">{{ p.description || '—' }}</div>
            <div class="pkg-actions">
              <el-button type="primary" :disabled="!channelEnabled" @click="buy(p, 'alipay')">
                支付宝
              </el-button>
              <el-button type="success" :disabled="!channelEnabled" @click="buy(p, 'wxpay')">
                微信
              </el-button>
            </div>
          </el-card>
        </el-col>
      </el-row>
    </div>

    <!-- 订单列表 -->
    <div class="card-block">
      <div class="flex-between" style="margin-bottom:10px">
        <h3 style="margin:0;font-size:15px">我的订单</h3>
        <div class="flex-wrap-gap">
          <el-select v-model="paging.status" placeholder="状态" clearable style="width:130px"
                     @change="() => { paging.offset = 0; loadOrders() }">
            <el-option label="全部" value="" />
            <el-option label="待支付" value="pending" />
            <el-option label="已到账" value="paid" />
            <el-option label="已取消" value="cancelled" />
            <el-option label="已超时" value="expired" />
          </el-select>
          <el-button @click="loadOrders" :loading="loadingOrder">
            <el-icon><Refresh /></el-icon> 刷新
          </el-button>
        </div>
      </div>

      <el-table :data="orders" stripe v-loading="loadingOrder">
        <el-table-column label="订单号" min-width="170">
          <template #default="{ row }">
            <code style="font-size:12px">{{ row.out_trade_no }}</code>
          </template>
        </el-table-column>
        <el-table-column label="套餐" min-width="120">
          <template #default="{ row }">{{ row.remark || `#${row.package_id}` }}</template>
        </el-table-column>
        <el-table-column label="金额" width="100">
          <template #default="{ row }">¥ {{ priceYuan(row.price_cny) }}</template>
        </el-table-column>
        <el-table-column label="积分" width="130">
          <template #default="{ row }">
            {{ formatCredit(row.credits + row.bonus) }}
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag :type="statusColor[row.status] || 'info'" size="small">
              {{ statusLabel[row.status] || row.status }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="170">
          <template #default="{ row }">{{ row.created_at }}</template>
        </el-table-column>
        <el-table-column label="操作" width="200" fixed="right">
          <template #default="{ row }">
            <el-button v-if="row.status === 'pending' && row.pay_url" type="primary" link
                       @click="() => openPayUrl(row.pay_url!)">继续支付</el-button>
            <el-button v-if="row.status === 'pending'" type="danger" link @click="cancel(row)">
              取消
            </el-button>
            <span v-if="row.status !== 'pending'" style="color:var(--el-text-color-placeholder)">—</span>
          </template>
        </el-table-column>
      </el-table>

      <el-pagination
        style="margin-top:12px"
        background
        layout="total, prev, pager, next"
        :total="total"
        v-model:current-page="currentPage"
        :page-size="paging.limit"
      />
    </div>
  </div>
</template>

<style scoped lang="scss">
.pkg-card {
  border-radius: 10px;
  transition: transform .15s;
  &:hover { transform: translateY(-2px); }
  .pkg-name { font-size: 16px; font-weight: 600; }
  .pkg-price {
    font-size: 14px; color: #f56c6c; margin: 8px 0 4px;
    span { font-size: 28px; font-weight: 700; }
  }
  .pkg-credit {
    font-size: 14px; color: var(--el-text-color-primary);
    .bonus { color: #67c23a; font-weight: 600; margin-left: 6px; }
  }
  .pkg-desc {
    font-size: 12px; color: var(--el-text-color-secondary);
    margin: 10px 0; min-height: 36px;
  }
  .pkg-actions { display: flex; gap: 8px; }
}
code {
  background: #f2f3f5;
  padding: 1px 6px;
  border-radius: 4px;
}
:global(html.dark) code { background: #1d2026; }
</style>
