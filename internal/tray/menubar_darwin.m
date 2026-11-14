//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#import <UserNotifications/UserNotifications.h>
#include "menubar_darwin.h"
#include "_cgo_export.h"

static uintptr_t handle;

@interface TrayTarget : NSObject
- (void)clicked:(NSMenuItem *)item;
@end

@implementation TrayTarget
- (void)clicked:(NSMenuItem *)item {
	trayClicked(handle, (int)item.tag);
}
@end

// TrayDelegate gets the reopen event, trebi:// links, and notification clicks.
@interface TrayDelegate : NSObject <NSApplicationDelegate, UNUserNotificationCenterDelegate>
@end

@implementation TrayDelegate
- (BOOL)applicationShouldHandleReopen:(NSApplication *)app hasVisibleWindows:(BOOL)visible {
	trayReopen(handle);
	return NO;
}

- (void)openURL:(NSAppleEventDescriptor *)event withReplyEvent:(NSAppleEventDescriptor *)reply {
	NSString *url = [[event paramDescriptorForKeyword:keyDirectObject] stringValue];
	if (url.length) {
		trayOpenLink(handle, (char *)url.UTF8String);
	}
}

- (void)userNotificationCenter:(UNUserNotificationCenter *)center
	willPresentNotification:(UNNotification *)notification
	  withCompletionHandler:(void (^)(UNNotificationPresentationOptions))done {
	done(UNNotificationPresentationOptionBanner | UNNotificationPresentationOptionSound);
}

- (void)userNotificationCenter:(UNUserNotificationCenter *)center
	didReceiveNotificationResponse:(UNNotificationResponse *)response
		 withCompletionHandler:(void (^)(void))done {
	NSString *route = response.notification.request.content.userInfo[@"route"];
	if ([response.actionIdentifier isEqualToString:UNNotificationDefaultActionIdentifier] && route.length) {
		trayNotificationClicked(handle, (char *)route.UTF8String);
	}
	done();
}
@end

static NSStatusItem *statusItem;
static TrayTarget *target;
static TrayDelegate *delegate;

// UNUserNotificationCenter throws outside a bundle, so a bare binary posts none.
static BOOL canNotify(void) {
	return NSBundle.mainBundle.bundleIdentifier.length > 0;
}

void trayRun(uintptr_t h) {
	handle = h;
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
		target = [TrayTarget new];
		delegate = [TrayDelegate new];
		NSApp.delegate = delegate;
		[[NSAppleEventManager sharedAppleEventManager] setEventHandler:delegate
			andSelector:@selector(openURL:withReplyEvent:)
			forEventClass:kInternetEventClass
			andEventID:kAEGetURL];
		if (canNotify()) {
			UNUserNotificationCenter *center = UNUserNotificationCenter.currentNotificationCenter;
			center.delegate = delegate;
		}
		statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
		statusItem.menu = [NSMenu new];
		statusItem.menu.autoenablesItems = NO;
		trayReady(handle);
		[NSApp run];
	}
}

// The favicon colors (oklch 0.526 0.199 268, dark 0.67 0.174 270.6).
static NSColor *brandColor(void) {
	NSAppearanceName name = [NSAppearance.currentDrawingAppearance
		bestMatchFromAppearancesWithNames:@[ NSAppearanceNameAqua, NSAppearanceNameDarkAqua ]];
	if ([name isEqualToString:NSAppearanceNameDarkAqua]) {
		return [NSColor colorWithSRGBRed:0.431 green:0.545 blue:1.0 alpha:1];
	}
	return [NSColor colorWithSRGBRed:0.231 green:0.357 blue:0.86 alpha:1];
}

// png is a shape mask. The handler paints it at draw time, so the color
// follows the menu bar appearance.
void traySetIcon(const void *png, int len, const char *title) {
	NSData *data = [NSData dataWithBytes:png length:len];
	NSString *text = [NSString stringWithUTF8String:title];
	dispatch_async(dispatch_get_main_queue(), ^{
		NSImage *mask = [[NSImage alloc] initWithData:data];
		NSImage *image = [NSImage imageWithSize:NSMakeSize(18, 18) flipped:NO drawingHandler:^BOOL(NSRect rect) {
			[mask drawInRect:rect];
			[brandColor() set];
			NSRectFillUsingOperation(rect, NSCompositingOperationSourceIn);
			return YES;
		}];
		statusItem.button.image = image;
		statusItem.button.imagePosition = text.length ? NSImageLeft : NSImageOnly;
		statusItem.button.title = text;
	});
}

// json: [{"text": "...", "id": 1}, {"separator": true}, ...]. id 0 is disabled.
void traySetMenu(const char *json) {
	NSData *data = [NSData dataWithBytes:json length:strlen(json)];
	dispatch_async(dispatch_get_main_queue(), ^{
		NSArray *items = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
		NSMenu *menu = statusItem.menu;
		[menu removeAllItems];
		for (NSDictionary *it in items) {
			if ([it[@"separator"] boolValue]) {
				[menu addItem:[NSMenuItem separatorItem]];
				continue;
			}
			NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:it[@"text"] action:nil keyEquivalent:@""];
			NSInteger tag = [it[@"id"] integerValue];
			if (tag > 0) {
				item.tag = tag;
				item.target = target;
				item.action = @selector(clicked:);
			}
			item.enabled = tag > 0;
			[menu addItem:item];
		}
	});
}

// Blocks until the user answers. Returns 1 for ok, 0 for cancel. Never call
// it on the main thread.
int trayAlert(const char *message, const char *info, const char *ok, const char *cancel) {
	NSString *m = [NSString stringWithUTF8String:message];
	NSString *i = [NSString stringWithUTF8String:info];
	NSString *o = [NSString stringWithUTF8String:ok];
	NSString *c = [NSString stringWithUTF8String:cancel];
	__block int result = 0;
	dispatch_sync(dispatch_get_main_queue(), ^{
		NSAlert *alert = [NSAlert new];
		alert.messageText = m;
		alert.informativeText = i;
		[alert addButtonWithTitle:o];
		if (c.length) {
			[alert addButtonWithTitle:c];
		}
		[NSApp activateIgnoringOtherApps:YES];
		result = [alert runModal] == NSAlertFirstButtonReturn ? 1 : 0;
	});
	return result;
}

void trayTerminate(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[NSApp terminate:nil];
	});
}

static BOOL isGranted(UNAuthorizationStatus status) {
	return status == UNAuthorizationStatusAuthorized || status == UNAuthorizationStatusProvisional;
}

// 0 not determined, 1 denied, 2 authorized.
static int statusCode(UNAuthorizationStatus status) {
	if (isGranted(status)) {
		return 2;
	}
	return status == UNAuthorizationStatusDenied ? 1 : 0;
}

// Never call it on the main thread. It waits for the settings read.
int trayNotificationAuthorization(void) {
	if (!canNotify()) {
		return 0;
	}
	__block int code = 0;
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	[UNUserNotificationCenter.currentNotificationCenter getNotificationSettingsWithCompletionHandler:^(UNNotificationSettings *settings) {
		code = statusCode(settings.authorizationStatus);
		dispatch_semaphore_signal(done);
	}];
	dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC));
	return code;
}

// Shows the permission prompt once. The user answer goes to Go.
void trayRequestNotifications(void) {
	if (!canNotify()) {
		return;
	}
	dispatch_async(dispatch_get_main_queue(), ^{
		[NSApp activateIgnoringOtherApps:YES];
		[UNUserNotificationCenter.currentNotificationCenter
			requestAuthorizationWithOptions:(UNAuthorizationOptionAlert | UNAuthorizationOptionSound | UNAuthorizationOptionBadge)
			completionHandler:^(BOOL granted, NSError *error) {
				if (error) {
					NSLog(@"trebi: notification request failed: %@", error);
				}
				trayNotificationStatus(handle, trayNotificationAuthorization());
			}];
	});
}

// With no grant, a notice is dropped without an error.
void trayNotify(const char *id, const char *title, const char *body, const char *route) {
	if (!canNotify()) {
		return;
	}
	UNMutableNotificationContent *content = [UNMutableNotificationContent new];
	content.title = [NSString stringWithUTF8String:title];
	content.body = [NSString stringWithUTF8String:body];
	content.sound = UNNotificationSound.defaultSound;
	content.userInfo = @{@"route" : [NSString stringWithUTF8String:route]};
	UNNotificationRequest *request = [UNNotificationRequest requestWithIdentifier:[NSString stringWithUTF8String:id]
		content:content
		trigger:nil];
	UNUserNotificationCenter *center = UNUserNotificationCenter.currentNotificationCenter;
	[center getNotificationSettingsWithCompletionHandler:^(UNNotificationSettings *settings) {
		if (isGranted(settings.authorizationStatus)) {
			[center addNotificationRequest:request withCompletionHandler:nil];
		}
	}];
}

void trayWithdraw(const char *id) {
	if (!canNotify()) {
		return;
	}
	NSArray *ids = @[ [NSString stringWithUTF8String:id] ];
	UNUserNotificationCenter *center = UNUserNotificationCenter.currentNotificationCenter;
	[center removeDeliveredNotificationsWithIdentifiers:ids];
	[center removePendingNotificationRequestsWithIdentifiers:ids];
}
