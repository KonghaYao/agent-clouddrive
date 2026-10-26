import ReactDOM from 'react-dom/client';
import { ConfigProvider, App as AntApp } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import { AuthProvider, completeSilentCallback, isSilentCallbackPage } from './auth';
import App from './App';
import './style.css';
// A silent-auth iframe must only answer its parent: never mount the app,
// create a business session, or start a second recovery attempt here.
if (isSilentCallbackPage()) {
  void completeSilentCallback();
} else {
  ReactDOM.createRoot(document.getElementById('root')!).render(
    <ConfigProvider locale={zhCN} theme={{ token: { colorPrimary: '#2c62e5', borderRadius: 10, fontFamily: 'Inter, -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", sans-serif', colorBgLayout: '#f5f7fb' } }}>
      <AntApp><AuthProvider><App /></AuthProvider></AntApp>
    </ConfigProvider>,
  );
}
