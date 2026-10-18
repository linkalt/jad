/**
 * @file TokenRequestDialog.tsx
 * @input Nothing; it registers itself as the presenter for the API token request
 * @output A modal with a masked field that answers the request, or null when it is dismissed
 * @position Mounted once at the root; replaces window.prompt so the token is not shown while typed
 */
import {useEffect, useRef, useState} from 'react';
import {Button, Dialog, HStack, Layout, LayoutContent, LayoutFooter, TextInput} from '@astryxdesign/core';
import {DialogHeader} from '@astryxdesign/core/Dialog';
import {registerTokenPresenter} from '../token-request';

export function TokenRequestDialog() {
  const [isOpen, setIsOpen] = useState(false);
  const [value, setValue] = useState('');
  const answerRef = useRef<((token: string | null) => void) | null>(null);

  useEffect(() => {
    const unregister = registerTokenPresenter(resolve => {
      answerRef.current?.(null);
      answerRef.current = resolve;
      setValue('');
      setIsOpen(true);
    });
    return () => {
      unregister();
      answerRef.current?.(null);
      answerRef.current = null;
    };
  }, []);

  const finish = (token: string | null) => {
    const answer = answerRef.current;
    answerRef.current = null;
    setIsOpen(false);
    setValue('');
    answer?.(token);
  };

  const token = value.trim();
  const submit = () => {
    if (token !== '') finish(token);
  };

  return (
    <Dialog isOpen={isOpen} onOpenChange={open => { if (!open) finish(null); }} purpose="form" aria-label="Backend API token">
      <Layout
        header={<DialogHeader title="Backend API token" subtitle="The server requires a token (JAD_API_TOKEN). It is kept for this tab only." />}
        content={
          <LayoutContent>
            <TextInput label="API token" type="password" autoComplete="off" value={value} onChange={setValue} onEnter={submit} hasAutoFocus />
          </LayoutContent>
        }
        footer={
          <LayoutFooter hasDivider>
            <HStack gap={2} justify="end" width="100%">
              <Button label="Cancel" variant="ghost" onClick={() => finish(null)} />
              <Button label="Continue" variant="primary" isDisabled={token === ''} onClick={submit} />
            </HStack>
          </LayoutFooter>
        }
      />
    </Dialog>
  );
}
