<script setup lang="ts">
import { reactive, ref, computed } from 'vue'
import type { FormInstance } from 'element-plus'
import { ElMessage } from 'element-plus'
import { useRouter, useRoute } from 'vue-router'
import { useUserStore } from '@/stores/user'
import { useSiteStore } from '@/stores/site'

const router = useRouter()
const route = useRoute()
const store = useUserStore()
const site = useSiteStore()

const siteName = computed(() => site.get('site.name', '积云'))
const siteDesc = computed(() =>
  site.get('site.description', '积云统一接口与控制台 · 多账号调度 · 图片生成 · 用量透明'),
)
const siteLogo = computed(() => site.get('site.logo_url', ''))
const siteFooter = computed(() => site.get('site.footer', ''))
const allowRegister = computed(() => site.allowRegister())

const formRef = ref<FormInstance>()
const loading = ref(false)

const form = reactive({
  email: '',
  password: '',
})

const rules = {
  email: [
    { required: true, message: '请输入邮箱', trigger: 'blur' },
    { type: 'email', message: '邮箱格式不正确', trigger: 'blur' },
  ],
  password: [
    { required: true, message: '请输入密码', trigger: 'blur' },
    { min: 6, message: '至少 6 位', trigger: 'blur' },
  ],
}

async function onSubmit() {
  if (!formRef.value) return
  const ok = await formRef.value.validate().catch(() => false)
  if (!ok) return
  loading.value = true
  try {
    await store.login(form.email, form.password)
    ElMessage.success('登录成功')
    const redirect = (route.query.redirect as string) || '/personal/dashboard'
    router.replace(redirect)
  } catch {
    // 错误已由 axios 拦截器 toast
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <div class="hero">
      <div class="brand">
        <img v-if="siteLogo" :src="siteLogo" class="logo-img" alt="logo" />
        <div v-else class="mark">{{ (siteName[0] || 'G').toUpperCase() }}</div>
        <h1>{{ siteName }} 控制台</h1>
      </div>
      <p class="tagline">{{ siteDesc }}</p>
      <ul class="features">
        <li><el-icon><Lightning /></el-icon> 多账号池 / 多代理池 · IMG2 终稿直出 · 批量出图 · 高并发调度</li>
        <li><el-icon><Lock /></el-icon> RBAC 权限 · 全链路审计 · 数据库一键备份 / 恢复</li>
        <li><el-icon><Medal /></el-icon> 积分钱包 · 预扣结算 · 易支付接入 · 用量透明</li>
      </ul>
    </div>
    <el-card class="form-card" shadow="hover">
      <div class="form-title">欢迎回来</div>
      <div class="form-sub">请使用管理员分配的账号登录</div>
      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        size="large"
        label-position="top"
        @submit.prevent="onSubmit"
      >
        <el-form-item label="邮箱" prop="email">
          <el-input v-model="form.email" placeholder="you@example.com" autocomplete="username">
            <template #prefix><el-icon><Message /></el-icon></template>
          </el-input>
        </el-form-item>
        <el-form-item label="密码" prop="password">
          <el-input v-model="form.password" type="password" show-password placeholder="至少 6 位"
                    autocomplete="current-password" @keyup.enter="onSubmit">
            <template #prefix><el-icon><Lock /></el-icon></template>
          </el-input>
        </el-form-item>
        <el-button type="primary" :loading="loading" class="submit" @click="onSubmit">登录</el-button>
        <div class="foot">
          <template v-if="allowRegister">
            还没有账号?<router-link to="/register">立即注册</router-link>
          </template>
          <template v-else>
            <span class="muted">管理员已关闭自助注册,请联系管理员创建账号</span>
          </template>
        </div>
      </el-form>
    </el-card>
    <div v-if="siteFooter" class="site-footer">{{ siteFooter }}</div>
  </div>
</template>

<style scoped lang="scss">
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-wrap: wrap;
  gap: 60px;
  padding: 40px 24px;
  box-sizing: border-box;
  background:
    radial-gradient(1000px 400px at 10% 20%, #a5c9ff66, transparent),
    radial-gradient(800px 400px at 90% 80%, #b1f1b288, transparent),
    linear-gradient(135deg, #eef5ff, #f9fffb);
}
:global(html.dark) .login-page {
  background:
    radial-gradient(1000px 400px at 10% 20%, #1b3a6a99, transparent),
    radial-gradient(800px 400px at 90% 80%, #1c4c2688, transparent),
    linear-gradient(135deg, #0d1117, #0b1f17);
}
:global(html.dark) .hero .tagline,
:global(html.dark) .hero .features { color: #cfd3dc; }
:global(html.dark) .hero h1 { color: #f2f3f5; }
.hero {
  flex: 0 1 420px;
  min-width: 0;
  max-width: 420px;
  .brand { display: flex; align-items: center; gap: 14px; }
  .logo-img { width: 48px; height: 48px; border-radius: 12px; object-fit: contain; background: #fff; }
  .mark {
    width: 48px; height: 48px; border-radius: 12px;
    display: inline-flex; align-items: center; justify-content: center;
    color: #fff; font-weight: 700; font-size: 18px;
    background: linear-gradient(135deg,#409eff,#67c23a);
  }
  h1 { font-size: 24px; margin: 0; color: #1f2330; }
  .tagline { color: #606266; margin-top: 6px; }
  .features {
    list-style: none; padding: 0; margin: 28px 0 0; color: #303133;
    li { display: flex; gap: 10px; align-items: center; margin-bottom: 12px; font-size: 14px; }
  }
}
.site-footer {
  position: absolute;
  bottom: 12px; left: 0; right: 0;
  text-align: center; font-size: 12px; color: #909399;
}
.foot .muted { color: #909399; }
.form-card {
  flex: 0 1 360px;
  width: 100%;
  max-width: 360px;
  min-width: 0;
  .form-title { font-size: 20px; font-weight: 700; margin-bottom: 4px; }
  .form-sub { color: var(--el-text-color-secondary); margin-bottom: 18px; font-size: 13px; }
  .submit { width: 100%; margin-top: 4px; }
  .foot { margin-top: 16px; text-align: center; font-size: 13px; color: var(--el-text-color-secondary); }
}

// 平板:图文与表单上下堆叠
@media (max-width: 960px) {
  .login-page { gap: 28px; padding: 32px 20px; }
  .hero { text-align: left; }
}

// 手机:隐藏左侧图文,表单占满
@media (max-width: 640px) {
  .login-page { padding: 24px 16px; gap: 0; }
  .hero { display: none; }
  .form-card { max-width: 100%; }
}
</style>
