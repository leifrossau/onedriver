#include "dbustypes.h"

#include <QDBusMetaType>

QDBusArgument &operator<<(QDBusArgument &arg, const RecentEntry &e)
{
    arg.beginStructure();
    arg << e.path << e.time << e.action;
    arg.endStructure();
    return arg;
}

const QDBusArgument &operator>>(const QDBusArgument &arg, RecentEntry &e)
{
    arg.beginStructure();
    arg >> e.path >> e.time >> e.action;
    arg.endStructure();
    return arg;
}

QDBusArgument &operator<<(QDBusArgument &arg, const TransferEntry &e)
{
    arg.beginStructure();
    arg << e.path << e.direction << e.state << e.done << e.total << e.queued;
    arg.endStructure();
    return arg;
}

const QDBusArgument &operator>>(const QDBusArgument &arg, TransferEntry &e)
{
    arg.beginStructure();
    arg >> e.path >> e.direction >> e.state >> e.done >> e.total >> e.queued;
    arg.endStructure();
    return arg;
}

void registerDBusTypes()
{
    qDBusRegisterMetaType<RecentEntry>();
    qDBusRegisterMetaType<QList<RecentEntry>>();
    qDBusRegisterMetaType<TransferEntry>();
    qDBusRegisterMetaType<QList<TransferEntry>>();
}
