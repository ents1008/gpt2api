import { createApp } from 'vue'
import { createPinia } from 'pinia'
import piniaPersist from 'pinia-plugin-persistedstate'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import * as ElementIcons from '@element-plus/icons-vue'

import App from './App.vue'
import router from './router'
import './styles/global.scss'
import { useSiteStore } from './stores/site'
import { printBrandToConsole, startBrandGuard } from './utils/brand'

const CHUNK_RELOAD_KEY = 'jiyun.chunk-reload-target'

function currentAppURL() {
  return `${window.location.pathname}${window.location.search}${window.location.hash}`
}

function markAndReload(target = currentAppURL()) {
  if (sessionStorage.getItem(CHUNK_RELOAD_KEY) === target) return
  sessionStorage.setItem(CHUNK_RELOAD_KEY, target)
  window.location.assign(target)
}

function clearReloadMarker() {
  if (sessionStorage.getItem(CHUNK_RELOAD_KEY) === currentAppURL()) {
    sessionStorage.removeItem(CHUNK_RELOAD_KEY)
  }
}

function isChunkLoadError(error: unknown) {
  const message = error instanceof Error ? error.message : String(error || '')
  const lower = message.toLowerCase()
  return (
    lower.includes('failed to fetch dynamically imported module') ||
    lower.includes('importing a module script failed') ||
    lower.includes('error loading dynamically imported module') ||
    lower.includes('unable to preload css')
  )
}

window.addEventListener('vite:preloadError', ((event: Event) => {
  event.preventDefault()
  markAndReload()
}) as EventListener)

router.onError((error, to) => {
  if (isChunkLoadError(error)) {
    markAndReload(to?.fullPath || currentAppURL())
  }
})

router.afterEach(() => {
  clearReloadMarker()
})

const app = createApp(App)

const pinia = createPinia()
pinia.use(piniaPersist)
app.use(pinia)
app.use(router)
app.use(ElementPlus, { size: 'default', locale: zhCn })

// 把 element icons 作为全局组件注册,模板里可直接 <el-icon><Setting /></el-icon>
for (const [name, comp] of Object.entries(ElementIcons)) {
  app.component(name, comp as never)
}

// 启动即异步拉取站点公开信息(匿名即可,失败静默),
// 用于登录页 / 注册页 / 顶栏统一展示 site.name / logo / 是否允许注册 等。
useSiteStore(pinia).refresh()

// 版权水印 + 防篡改守卫(不要删除)
printBrandToConsole()
startBrandGuard()

app.mount('#app')
