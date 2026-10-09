export type DialogTone = 'danger' | 'warning' | 'info' | 'primary';

export type ConfirmOptions = {
  title: string;
  description: string;
  tone?: DialogTone;
  icon?: string;
  confirmText?: string;
  cancelText?: string;
  hideCancel?: boolean;
  action: () => Promise<void> | void;
};
