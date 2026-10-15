import './storage-migration-run';
import {StrictMode} from 'react';
import {createRoot} from 'react-dom/client';
import {Theme} from '@astryxdesign/core/theme';
import './index.css';
import App from './App';
import appConfig from './config.json';
import {useColorScheme} from './color-scheme';
import {neutralTheme} from './themes/neutral/neutral';

function Root() {
  const [colorScheme, toggleColorScheme] = useColorScheme(appConfig.color_scheme_storage_key);
  return (
    <Theme theme={neutralTheme} mode={colorScheme}>
      <App colorScheme={colorScheme} onToggleColorScheme={toggleColorScheme} />
    </Theme>
  );
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
);
