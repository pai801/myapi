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
          {systemName}{' '}
          {t('footer.license')}{' '}
          <a href='https://opensource.org/licenses/mit-license.php'>
            {t('footer.mit')}
          </a>
        </div>
      </Container>
    </Segment>
  );
};

export default Footer;
