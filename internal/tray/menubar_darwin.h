#include <stdint.h>

void trayRun(uintptr_t handle);
void traySetIcon(const void *png, int len, const char *title);
void traySetMenu(const char *json);
int trayAlert(const char *message, const char *info, const char *ok, const char *cancel);
void trayTerminate(void);
