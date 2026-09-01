export interface ActiveStatus {
  isWorking: boolean;
  parserIcon: string;
  parserName: string;
  terminalStatus: string;
}

export const INITIAL_STATUS: ActiveStatus = {
  isWorking: false,
  parserIcon: '⬛',
  parserName: 'Terminal',
  terminalStatus: 'connecting'
};
