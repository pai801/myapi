import React, { useContext, useEffect, useState, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { Card, Grid, Header } from 'semantic-ui-react';
import { API, showError, timestamp2string } from '../../helpers';
import { StatusContext } from '../../context/Status';
import { UserContext } from '../../context/User';
import { Link } from 'react-router-dom';

const Home = () => {
  const { t } = useTranslation();
  const [statusState, statusDispatch] = useContext(StatusContext);
  const [userState] = useContext(UserContext);
  const [homePageContent, setHomePageContent] = useState('');
  const [homePageContentLoaded, setHomePageContentLoaded] = useState(false);

  const displayHomePageContent = async () => {
    try {
      const res = await API.get('/api/home_page_content');
      const { success, message, data } = res.data;
      if (success) {
        if (data && data !== '') {
          setHomePageContent(data);
        }
      } else {
        showError(message);
      }
    } catch (error) {
      showError(error.message);
    }
    setHomePageContentLoaded(true);
  };

  const initRef = useRef(false);

  useEffect(() => {
    if (initRef.current) return;
    initRef.current = true;
    displayHomePageContent().then();
  }, []);

  const getStartTimeString = () => {
    const timestamp = statusState?.status?.start_time;
    return timestamp2string(timestamp);
  };

  return (
    <div className='dashboard-container'>
          <Card fluid className='chart-card'>
            <Card.Content>
              <Card.Header className='header'>
                {t('home.welcome.title')}
              </Card.Header>
              <Card.Description style={{ lineHeight: '1.6' }}>
                <p>{t('home.welcome.description')}</p>
                {!userState.user && <p>{t('home.welcome.login_notice')}</p>}
              </Card.Description>
            </Card.Content>
          </Card>
          {homePageContentLoaded && homePageContent !== '' && (
            <div dangerouslySetInnerHTML={{ __html: homePageContent }}></div>
          )}
          <Card fluid className='chart-card'>
            <Card.Content>
              <Card.Header>
                <Header as='h3'>{t('home.system_status.title')}</Header>
              </Card.Header>
              <Grid columns={2} stackable>
                <Grid.Column>
                  <Card
                    fluid
                    className='chart-card'
                    style={{ boxShadow: '0 1px 3px rgba(0,0,0,0.12)' }}
                  >
                    <Card.Content>
                      <Card.Header>
                        <Header as='h3' style={{ color: '#444' }}>
                          {t('home.system_status.info.title')}
                        </Header>
                      </Card.Header>
                      <Card.Description
                        style={{ lineHeight: '2', marginTop: '1em' }}
                      >
                        <p
                          style={{
                            display: 'flex',
                            alignItems: 'center',
                            gap: '0.5em',
                          }}
                        >
                          <i className='info circle icon'></i>
                          <span style={{ fontWeight: 'bold' }}>
                            {t('home.system_status.info.name')}
                          </span>
                          <span>{statusState?.status?.system_name}</span>
                        </p>
                        <p
                          style={{
                            display: 'flex',
                            alignItems: 'center',
                            gap: '0.5em',
                          }}
                        >
                          <i className='code branch icon'></i>
                          <span style={{ fontWeight: 'bold' }}>
                            {t('home.system_status.info.version')}
                          </span>
                          <span>
                            {statusState?.status?.version || 'unknown'}
                          </span>
                        </p>
                        <p
                          style={{
                            display: 'flex',
                            alignItems: 'center',
                            gap: '0.5em',
                          }}
                        >
                          <i className='github icon'></i>
                          <span style={{ fontWeight: 'bold' }}>
                            {t('home.system_status.info.source')}
                          </span>
                          <a
                            href='https://github.com/songquanpeng/one-api'
                            target='_blank'
                            style={{ color: '#2185d0' }}
                          >
                            {t('home.system_status.info.source_link')}
                          </a>
                        </p>
                        <p
                          style={{
                            display: 'flex',
                            alignItems: 'center',
                            gap: '0.5em',
                          }}
                        >
                          <i className='clock outline icon'></i>
                          <span style={{ fontWeight: 'bold' }}>
                            {t('home.system_status.info.start_time')}
                          </span>
                          <span>{getStartTimeString()}</span>
                        </p>
                      </Card.Description>
                    </Card.Content>
                  </Card>
                </Grid.Column>

                <Grid.Column>
                  <Card
                    fluid
                    className='chart-card'
                    style={{ boxShadow: '0 1px 3px rgba(0,0,0,0.12)' }}
                  >
                    <Card.Content>
                      <Card.Header>
                        <Header as='h3' style={{ color: '#444' }}>
                          {t('home.system_status.config.title')}
                        </Header>
                      </Card.Header>
                      <Card.Description
                        style={{ lineHeight: '2', marginTop: '1em' }}
                      >
                        <p
                          style={{
                            display: 'flex',
                            alignItems: 'center',
                            gap: '0.5em',
                          }}
                        >
                          <i className='shield alternate icon'></i>
                          <span style={{ fontWeight: 'bold' }}>
                            {t('home.system_status.config.turnstile')}
                          </span>
                          <span
                            style={{
                              color: statusState?.status?.turnstile_check
                                ? '#21ba45'
                                : '#db2828',
                              fontWeight: '500',
                            }}
                          >
                            {statusState?.status?.turnstile_check
                              ? t('home.system_status.config.enabled')
                              : t('home.system_status.config.disabled')}
                          </span>
                        </p>
                      </Card.Description>
                    </Card.Content>
                  </Card>
                </Grid.Column>
              </Grid>
            </Card.Content>
          </Card>
        </div>
  );
};

export default Home;
