import React from 'react';
import { useTranslation } from 'react-i18next';
import { Container, Segment } from 'semantic-ui-react';
import { getSystemName } from '../helpers';

const Footer = () => {
  const { t } = useTranslation();
  const systemName = getSystemName();

  return (
    <Segment vertical>
      <Container textAlign='center' style={{ color: '#666666' }}>
        <div className='custom-footer'>
          <a href='https://github.com/pai801/myapi' target='_blank' rel='noopener noreferrer'>
            {systemName}
          </a>{' '}
          {t('footer.license_before')}{' '}
          <a href='https://github.com/songquanpeng/one-api' target='_blank' rel='noopener noreferrer'>
            {t('footer.one_api')}
          </a>{' '}
          {t('footer.license_after')}{' '}
          <a href='https://opensource.org/licenses/mit-license.php' target='_blank' rel='noopener noreferrer'>
            {t('footer.mit')}
          </a>
        </div>
      </Container>
    </Segment>
  );
};

export default Footer;
