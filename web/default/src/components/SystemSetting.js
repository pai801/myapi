import React, { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Divider,
  Form,
  Grid,
  Header,
  Modal,
} from 'semantic-ui-react';
import { API, removeTrailingSlash, showError } from '../helpers';

const SystemSetting = () => {
  const { t } = useTranslation();
  let [inputs, setInputs] = useState({
    PasswordLoginEnabled: '',
    ServerAddress: '',
    TurnstileCheckEnabled: '',
    TurnstileSiteKey: '',
    TurnstileSecretKey: '',
  });
  const [originInputs, setOriginInputs] = useState({});
  let [loading, setLoading] = useState(false);
  const [showPasswordWarningModal, setShowPasswordWarningModal] =
    useState(false);

  const getOptions = async () => {
    const res = await API.get('/api/option/');
    const { success, message, data } = res.data;
    if (success) {
      let newInputs = {};
      data.forEach((item) => {
        newInputs[item.key] = item.value;
      });
      setInputs({
        ...newInputs,
      });
      setOriginInputs(newInputs);
    } else {
      showError(message);
    }
  };

  useEffect(() => {
    getOptions().then();
  }, []);

  const updateOption = async (key, value) => {
    setLoading(true);
    switch (key) {
      case 'PasswordLoginEnabled':
      case 'TurnstileCheckEnabled':
        value = inputs[key] === 'true' ? 'false' : 'true';
        break;
      default:
        break;
    }
    const res = await API.put('/api/option/', {
      key,
      value,
    });
    const { success, message } = res.data;
    if (success) {
      setInputs((inputs) => ({
        ...inputs,
        [key]: value,
      }));
    } else {
      showError(message);
    }
    setLoading(false);
  };

  const handleInputChange = async (e, { name, value }) => {
    if (name === 'PasswordLoginEnabled' && inputs[name] === 'true') {
      // block disabling password login
      setShowPasswordWarningModal(true);
      return;
    }
    if (
      name === 'ServerAddress' ||
      name === 'TurnstileSiteKey' ||
      name === 'TurnstileSecretKey'
    ) {
      setInputs((inputs) => ({ ...inputs, [name]: value }));
    } else {
      await updateOption(name, value);
    }
  };

  const submitSystemName = async () => {
    await updateOption('SystemName', inputs.SystemName);
  };

  const submitServerAddress = async () => {
    let ServerAddress = removeTrailingSlash(inputs.ServerAddress);
    await updateOption('ServerAddress', ServerAddress);
  };

  const submitTurnstile = async () => {
    if (originInputs['TurnstileSiteKey'] !== inputs.TurnstileSiteKey) {
      await updateOption('TurnstileSiteKey', inputs.TurnstileSiteKey);
    }
    if (
      originInputs['TurnstileSecretKey'] !== inputs.TurnstileSecretKey &&
      inputs.TurnstileSecretKey !== ''
    ) {
      await updateOption('TurnstileSecretKey', inputs.TurnstileSecretKey);
    }
  };

  return (
    <Grid columns={1}>
      <Grid.Column>
        <Form loading={loading}>
          <Header as='h3'>{t('setting.system.general.title')}</Header>
          <Form.Group widths='equal'>
            <Form.Input
              label={t('setting.system.general.system_name')}
              placeholder={t(
                'setting.system.general.system_name_placeholder'
              )}
              value={inputs.SystemName || ''}
              name='SystemName'
              onChange={handleInputChange}
            />
          </Form.Group>
          <Form.Button onClick={submitSystemName}>
            {t('setting.system.general.buttons.save_name')}
          </Form.Button>

          <Form.Group widths='equal'>
            <Form.Input
              label={t('setting.system.general.server_address')}
              placeholder={t(
                'setting.system.general.server_address_placeholder'
              )}
              value={inputs.ServerAddress}
              name='ServerAddress'
              onChange={handleInputChange}
            />
          </Form.Group>
          <Form.Button onClick={submitServerAddress}>
            {t('setting.system.general.buttons.update')}
          </Form.Button>

          <Divider />
          <Header as='h3'>{t('setting.system.turnstile.title')}
            <Header.Subheader>
              {t('setting.system.turnstile.subtitle')}
              <a href='https://dash.cloudflare.com/' target='_blank'>
                {t('setting.system.turnstile.manage_link')}
              </a>
              {t('setting.system.turnstile.manage_text')}
            </Header.Subheader>
          </Header>
          <Form.Group inline>
            <Form.Checkbox
              checked={inputs.TurnstileCheckEnabled === 'true'}
              label={t('setting.system.login.turnstile')}
              name='TurnstileCheckEnabled'
              onChange={handleInputChange}
            />
          </Form.Group>
          <Form.Group widths={3}>
            <Form.Input
              label={t('setting.system.turnstile.site_key')}
              name='TurnstileSiteKey'
              onChange={handleInputChange}
              autoComplete='new-password'
              value={inputs.TurnstileSiteKey}
              placeholder={t('setting.system.turnstile.site_key_placeholder')}
            />
            <Form.Input
              label={t('setting.system.turnstile.secret_key')}
              name='TurnstileSecretKey'
              onChange={handleInputChange}
              type='password'
              autoComplete='new-password'
              value={inputs.TurnstileSecretKey}
              placeholder={t('setting.system.turnstile.secret_key_placeholder')}
            />
          </Form.Group>
          <Form.Button onClick={submitTurnstile}>
            {t('setting.system.turnstile.buttons.save')}
          </Form.Button>
        </Form>
      </Grid.Column>
    </Grid>
  );
};

export default SystemSetting;
