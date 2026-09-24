import ReactDOM from 'react-dom/client'
import '@astryxdesign/core/reset.css'
import '@astryxdesign/core/astryx.css'
import '@astryxdesign/theme-neutral/theme.css'
import { Theme } from '@astryxdesign/core/theme'
import { InternationalizationProvider } from '@astryxdesign/core/i18n'
import zhCN from '@astryxdesign/core/locales/zh-CN.json'
import { neutralTheme } from '@astryxdesign/theme-neutral/built'
import './styles.css'
import { App } from './App'

ReactDOM.createRoot(document.getElementById('root')!).render(
  <InternationalizationProvider locale="zh-CN" messages={{ 'zh-CN': zhCN }}><Theme theme={neutralTheme} mode="light"><App /></Theme></InternationalizationProvider>,
)
