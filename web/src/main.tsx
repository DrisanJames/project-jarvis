import React from 'react'
import ReactDOM from 'react-dom/client'
import { capturePrerender } from './prerenderSnapshot'
import App from './App'
import './styles/jarvis-tokens.css'
import './styles/index.css'

const root = document.getElementById('root')!
capturePrerender(root)

ReactDOM.createRoot(root).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
