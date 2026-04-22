<script setup lang="ts">
import { brandParts } from '@/utils/brand'

const brand = brandParts()
const brandQQHref = `https://qm.qq.com/q/${brand.qq}`
</script>

<template>
  <div class="blank-layout">
    <router-view />
    <div class="blank-footer">
      <b class="brand-name">{{ brand.brand }}</b>
      <span class="sep">{{ brand.sep }}</span>
      <span>{{ brand.qqLabel }}</span>
      <a :href="brandQQHref" target="_blank" rel="noopener" class="footer-link">{{ brand.qq }}</a>
    </div>
  </div>
</template>

<style scoped>
.blank-layout {
  min-height: 100vh;
  position: relative;
  padding-bottom: 40px;
  box-sizing: border-box;
}
.blank-footer {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  text-align: center;
  padding: 10px 12px;
  font-size: 12px;
  color: var(--el-text-color-secondary, #909399);
  line-height: 1.6;
}
.blank-footer .brand-name {
  color: var(--el-color-primary, #409eff);
  letter-spacing: 0.5px;
  margin-right: 4px;
}
.blank-footer .sep {
  color: var(--el-text-color-disabled, #c0c4cc);
  margin: 0 4px;
  user-select: none;
}
.footer-link {
  color: var(--el-color-primary, #409eff);
  text-decoration: none;
  margin: 0 2px;
}
.footer-link:hover { text-decoration: underline; }
</style>
