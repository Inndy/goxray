<script setup>
import { NConfigProvider, NGlobalStyle, darkTheme } from 'naive-ui'

// Dense, dark, monospace-leaning: this is a tool for reading a lot of text at
// once, so the theme overrides trim naive-ui's default padding and font sizes.
const themeOverrides = {
  common: {
    fontFamily:
      'ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif',
    fontFamilyMono: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
    fontSize: '12px',
    fontSizeSmall: '11px',
    primaryColor: '#58a6ff',
    primaryColorHover: '#79b8ff',
    borderRadius: '3px',
  },
  Collapse: { titleFontSize: '12px', itemMargin: '6px 0 0 0' },
  Tabs: { tabFontSizeSmall: '11px', tabPaddingSmallLine: '5px 10px' },
  Tag: { heightTiny: '16px', fontSizeTiny: '10px' },
}
</script>

<template>
  <NConfigProvider :theme="darkTheme" :theme-overrides="themeOverrides">
    <NGlobalStyle />
    <RouterView />
  </NConfigProvider>
</template>
